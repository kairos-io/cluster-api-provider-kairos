// Kubevirt-env CLI. Run e.g.:
//
//	go run ./cmd/kubevirt-env/
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubevirtenv"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const defaultClusterName = "kairos-capi-test"

func main() {
	rootCmd := &cobra.Command{
		Use:   "kubevirt-env",
		Short: "Provision a local kind + KubeVirt management cluster and install the full demo stack",
		Long:  "Creates the work directory (.work-kubevirt-<cluster-name>/), downloads pinned CLIs into <workdir>/bin, creates the kind cluster, and installs components in order (same flow as library RunFullDemoSetup).",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := kubevirtEnv()
			if err != nil {
				return err
			}
			return kubevirtenv.RunFullDemoSetup(context.Background(), env)
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return initializeConfig()
		},
	}

	rootCmd.PersistentFlags().String("cluster-name", defaultClusterName, "Cluster name (can also be set via CLUSTER_NAME env var)")
	_ = viper.BindPFlag("cluster-name", rootCmd.PersistentFlags().Lookup("cluster-name"))
	_ = viper.BindEnv("cluster-name", "CLUSTER_NAME")

	rootCmd.AddCommand(&cobra.Command{
		Use:   "cleanup",
		Short: "Delete the kind cluster and remove the work directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := kubevirtEnv()
			if err != nil {
				return err
			}
			return runCleanup(env)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func initializeConfig() error {
	viper.SetEnvPrefix("")
	viper.AutomaticEnv()
	return nil
}

func getClusterName() string {
	return viper.GetString("cluster-name")
}

func getWorkDir() string {
	return ".work-kubevirt-" + getClusterName()
}

func kubevirtEnv() (*kubevirtenv.Environment, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	repoRoot, err := kubevirtenv.FindRepoRoot(wd)
	if err != nil {
		return nil, fmt.Errorf("repo root: %w", err)
	}
	return &kubevirtenv.Environment{
		ClusterName: getClusterName(),
		WorkDir:     getWorkDir(),
		RepoRoot:    repoRoot,
		Logger:      kubevirtenv.StdLogger{},
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	}, nil
}

func runCleanup(env *kubevirtenv.Environment) error {
	log := env.Logger
	ctx := context.Background()
	log.Step("=== Cleaning up ===")
	log.Infof("Cluster name: %s", env.ClusterName)
	log.Step("Deleting kind cluster...")
	if err := env.DeleteKindCluster(ctx); err != nil {
		log.Warnf("delete kind cluster: %v", err)
	}
	log.Step("Removing work directory...")
	if err := env.RemoveWorkDir(); err != nil {
		log.Warnf("remove work dir: %v", err)
	}
	log.Step("Cleanup complete ✓")
	return nil
}
