package pkg

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/appscode/go/flags"
	"github.com/appscode/go/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	appcatalog_cs "kmodules.xyz/custom-resources/client/clientset/versioned"
	"kubedb.dev/apimachinery/apis/config/v1alpha1"
	api_v1beta1 "stash.appscode.dev/stash/apis/stash/v1beta1"
	"stash.appscode.dev/stash/pkg/restic"
	"stash.appscode.dev/stash/pkg/util"
)

func NewCmdRestore() *cobra.Command {
	var (
		masterURL      string
		kubeconfigPath string
		opt            = mongoOptions{
			setupOptions: restic.SetupOptions{
				ScratchDir:  restic.DefaultScratchDir,
				EnableCache: false,
			},
			defaultDumpOptions: restic.DumpOptions{
				Host:     restic.DefaultHost,
				FileName: MongoDumpFile,
			},
		}
	)

	cmd := &cobra.Command{
		Use:               "restore-mongo",
		Short:             "Restores MongoDB Backup",
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			flags.EnsureRequiredFlags(cmd, "appbinding", "provider", "secret-dir")

			// prepare client
			config, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfigPath)
			if err != nil {
				return err
			}
			opt.kubeClient, err = kubernetes.NewForConfig(config)
			if err != nil {
				return err
			}
			opt.catalogClient, err = appcatalog_cs.NewForConfig(config)
			if err != nil {
				return err
			}

			var restoreOutput *restic.RestoreOutput
			restoreOutput, err = opt.restoreMongoDB()
			if err != nil {
				restoreOutput = &restic.RestoreOutput{
					HostRestoreStats: []api_v1beta1.HostRestoreStats{
						{
							Hostname: opt.defaultDumpOptions.Host,
							Phase:    api_v1beta1.HostRestoreFailed,
							Error:    err.Error(),
						},
					},
				}
			}
			// If output directory specified, then write the output in "output.json" file in the specified directory
			if opt.outputDir != "" {
				return restoreOutput.WriteOutput(filepath.Join(opt.outputDir, restic.DefaultOutputFileName))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&opt.mongoArgs, "mongo-args", opt.mongoArgs, "Additional arguments")

	cmd.Flags().StringVar(&masterURL, "master", masterURL, "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", kubeconfigPath, "Path to kubeconfig file with authorization information (the master location is set by the master flag).")
	cmd.Flags().StringVar(&opt.namespace, "namespace", "default", "Namespace of Backup/Restore Session")
	cmd.Flags().StringVar(&opt.appBindingName, "appbinding", opt.appBindingName, "Name of the app binding")
	cmd.Flags().IntVar(&opt.maxConcurrency, "max-concurrency", 3, "maximum concurrent backup process to run to take backup from each replicasets")

	cmd.Flags().StringVar(&opt.setupOptions.Provider, "provider", opt.setupOptions.Provider, "Backend provider (i.e. gcs, s3, azure etc)")
	cmd.Flags().StringVar(&opt.setupOptions.Bucket, "bucket", opt.setupOptions.Bucket, "Name of the cloud bucket/container (keep empty for local backend)")
	cmd.Flags().StringVar(&opt.setupOptions.Endpoint, "endpoint", opt.setupOptions.Endpoint, "Endpoint for s3/s3 compatible backend or REST server URL")
	cmd.Flags().StringVar(&opt.setupOptions.Path, "path", opt.setupOptions.Path, "Directory inside the bucket where backup will be stored")
	cmd.Flags().StringVar(&opt.setupOptions.SecretDir, "secret-dir", opt.setupOptions.SecretDir, "Directory where storage secret has been mounted")
	cmd.Flags().StringVar(&opt.setupOptions.ScratchDir, "scratch-dir", opt.setupOptions.ScratchDir, "Temporary directory")
	cmd.Flags().BoolVar(&opt.setupOptions.EnableCache, "enable-cache", opt.setupOptions.EnableCache, "Specify whether to enable caching for restic")
	cmd.Flags().IntVar(&opt.setupOptions.MaxConnections, "max-connections", opt.setupOptions.MaxConnections, "Specify maximum concurrent connections for GCS, Azure and B2 backend")

	cmd.Flags().StringVar(&opt.defaultDumpOptions.Host, "hostname", opt.defaultDumpOptions.Host, "Name of the host machine")
	cmd.Flags().StringVar(&opt.defaultDumpOptions.SourceHost, "source-hostname", opt.defaultDumpOptions.SourceHost, "Name of the host whose data will be restored")
	cmd.Flags().StringVar(&opt.defaultDumpOptions.Snapshot, "snapshot", opt.defaultDumpOptions.Snapshot, "Snapshot to dump")

	cmd.Flags().StringVar(&opt.outputDir, "output-dir", opt.outputDir, "Directory where output.json file will be written (keep empty if you don't need to write output in file)")

	return cmd
}

func (opt *mongoOptions) restoreMongoDB() (*restic.RestoreOutput, error) {
	// apply nice, ionice settings from env
	var err error
	opt.setupOptions.Nice, err = util.NiceSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	opt.setupOptions.IONice, err = util.IONiceSettingsFromEnv()
	if err != nil {
		return nil, err
	}

	// get app binding
	appBinding, err := opt.catalogClient.AppcatalogV1alpha1().AppBindings(opt.namespace).Get(opt.appBindingName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	// get secret
	appBindingSecret, err := opt.kubeClient.CoreV1().Secrets(opt.namespace).Get(appBinding.Spec.Secret.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// unmarshal parameter is the field has value
	parameters := v1alpha1.MongoDBConfiguration{}
	if appBinding.Spec.Parameters != nil {
		if err = json.Unmarshal(appBinding.Spec.Parameters.Raw, &parameters); err != nil {
			log.Errorf("unable to unmarshal appBinding.Spec.Parameters.Raw. Reason: %v", err)
		}
	}

	if appBinding.Spec.ClientConfig.CABundle != nil {
		if err := ioutil.WriteFile(filepath.Join(opt.setupOptions.ScratchDir, MongoTLSCertFileName), appBinding.Spec.ClientConfig.CABundle, os.ModePerm); err != nil {
			return nil, errors.Wrap(err, "failed to write key for CA certificate")
		}
		adminCreds = []interface{}{
			"--ssl",
			"--sslCAFile", filepath.Join(opt.setupOptions.ScratchDir, MongoTLSCertFileName),
		}

		// get certificate secret to get client certificate
		data, ok := appBindingSecret.Data[MongoClientPemFileName]
		if !ok {
			return nil, errors.Wrap(err, "unable to get client certificate from secret.")
		}
		if err := ioutil.WriteFile(filepath.Join(opt.setupOptions.ScratchDir, MongoClientPemFileName), data, os.ModePerm); err != nil {
			return nil, errors.Wrap(err, "failed to write client certificate")
		}
		user, err := getSSLUser(filepath.Join(opt.setupOptions.ScratchDir, MongoClientPemFileName))
		if err != nil {
			return nil, errors.Wrap(err, "unable to get user from ssl.")
		}
		adminCreds = append(adminCreds, []interface{}{
			"--sslPEMKeyFile", filepath.Join(opt.setupOptions.ScratchDir, MongoClientPemFileName),
			"-u", user,
			"--authenticationMechanism", "MONGODB-X509",
			"--authenticationDatabase", "$external",
		}...)

	} else {
		adminCreds = []interface{}{
			"--username", string(appBindingSecret.Data[MongoUserKey]),
			"--password", string(appBindingSecret.Data[MongoPasswordKey]),
			"--authenticationDatabase", "admin",
		}
	}

	getDumpOpts := func(mongoDSN, hostKey string, isStandalone bool) restic.DumpOptions {
		log.Infoln("processing backupOptions for ", mongoDSN)
		dumpOpt := restic.DumpOptions{
			Host:       hostKey,
			SourceHost: hostKey,
			FileName:   opt.defaultDumpOptions.FileName,
			Snapshot:   opt.defaultDumpOptions.Snapshot,
		}

		// setup pipe command
		dumpOpt.StdoutPipeCommand = restic.Command{
			Name: MongoRestoreCMD,
			Args: append([]interface{}{
				"--host", mongoDSN,
				"--archive",
			}, adminCreds...),
		}

		if isStandalone {
			dumpOpt.StdoutPipeCommand.Args = append(dumpOpt.StdoutPipeCommand.Args, "--port="+fmt.Sprint(appBinding.Spec.ClientConfig.Service.Port))
		} else {
			// - port is already added in mongoDSN with replicasetName/host:port format.
			// - oplog is enabled automatically for replicasets.
			dumpOpt.StdoutPipeCommand.Args = append(dumpOpt.StdoutPipeCommand.Args, "--oplogReplay")
		}
		if opt.mongoArgs != "" {
			dumpOpt.StdoutPipeCommand.Args = append(dumpOpt.StdoutPipeCommand.Args, opt.mongoArgs)
		}
		return dumpOpt
	}

	// set opt.maxConcurrency
	if len(parameters.ReplicaSets) <= 1 {
		opt.maxConcurrency = 1
	}

	// If parameters.ReplicaSets is not empty, then replicaset hosts are given in key:value pair,
	// where, keys are host-0,host-1 etc and values are the replicaset dsn of one replicaset component
	//
	// Procedure of restore in a sharded or replicaset cluster
	// - Restore the CSRS primary mongod data files.
	// - Restore Each Shard Replica Set
	// ref: https://docs.mongodb.com/manual/tutorial/backup-sharded-cluster-with-database-dumps/

	if parameters.ConfigServer != "" {
		opt.dumpOptions = append(opt.dumpOptions, getDumpOpts(parameters.ConfigServer, MongoConfigSVRHostKey, false))
	}

	for key, host := range parameters.ReplicaSets {
		opt.dumpOptions = append(opt.dumpOptions, getDumpOpts(host, key, false))
	}

	// if parameters.ReplicaSets is nil, then perform normal backup with clientconfig.Service.Name mongo dsn
	if parameters.ReplicaSets == nil {
		opt.dumpOptions = append(opt.dumpOptions, getDumpOpts(appBinding.Spec.ClientConfig.Service.Name, restic.DefaultHost, true))
	}

	log.Infoln("processing restore.")

	// wait for DB ready
	waitForDBReady(appBinding.Spec.ClientConfig.Service.Name, appBinding.Spec.ClientConfig.Service.Port)

	// init restic wrapper
	resticWrapper, err := restic.NewResticWrapper(opt.setupOptions)
	if err != nil {
		return nil, err
	}
	// hide password, don't print cmd
	resticWrapper.HideCMD()

	// Run dump
	return resticWrapper.ParallelDump(opt.dumpOptions, opt.maxConcurrency)
}
