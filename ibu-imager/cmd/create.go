/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/openshift/api/config/v1"
	cp "github.com/otiai10/copy"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	seed "github.com/openshift-kni/lifecycle-agent/ibu-imager/seedcreator"
)

var (
	scheme = runtime.NewScheme()
	// authFile is the path to the registry credentials used to push the OCI image
	authFile string
	// containerRegistry is the registry to push the OCI image
	containerRegistry string
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// recertContainerImage is the container image for the recert tool
var recertContainerImage string

// createCmd represents the create command
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create OCI image and push it to a container registry.",
	Run: func(cmd *cobra.Command, args []string) {
		create()
	},
}

func init() {

	// Add create command
	rootCmd.AddCommand(createCmd)

	// Add flags related to container registry
	createCmd.Flags().StringVarP(&authFile, "authfile", "a", imageRegistryAuthFile, "The path to the authentication file of the container registry.")
	createCmd.Flags().StringVarP(&containerRegistry, "image", "i", "", "The full image name with the container registry to push the OCI image.")
	createCmd.Flags().StringVarP(&recertContainerImage, "recert-image", "e", defaultRecertImage, "The full image name for the recert container tool.")
}

func create() {

	var err error
	log.Printf("OCI image creation has started")

	// Check if containerRegistry was provided by the user
	if containerRegistry == "" {
		fmt.Printf(" *** Please provide a valid container registry to store the created OCI images *** \n")
		log.Info("Skipping OCI image creation.")
		return
	}

	op := ops.NewOps(log, ops.NewExecutor(log, true))
	rpmOstreeClient := ostree.NewClient("ibu-imager", op)

	err = copyConfigurationFiles(op)
	if err != nil {
		log.Fatal("Failed to add configuration files", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigFile)
	if err != nil {
		log.Fatal("failed top create k8s config", err)
	}

	client, err := runtimeClient.New(config, runtimeClient.Options{Scheme: scheme})
	if err != nil {
		log.Fatal("failed to create runtime client", err)
	}

	seedCreator := seed.NewSeedCreator(client, log, op, rpmOstreeClient, backupDir, kubeconfigFile,
		containerRegistry, authFile, recertContainerImage)
	err = seedCreator.CreateSeedImage()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("OCI image created successfully!")
}

// TODO: move those functions to seed creator and add cleanup
func copyConfigurationFiles(ops ops.Ops) error {
	// copy scripts
	err := copyConfigurationScripts()
	if err != nil {
		return err
	}

	return handleServices(ops)
}

func copyConfigurationScripts() error {
	log.Infof("Copying installation_configuration_files/scripts to local/bin")
	return cp.Copy("installation_configuration_files/scripts", "/var/usrlocal/bin", cp.Options{AddPermission: os.FileMode(0o777)})
}

// copyEnvFile reads and copy the env file from the source directory to the destination directory
// while performing variable substitution for each var.

func handleServices(ops ops.Ops) error {
	dir := "installation_configuration_files/services"
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		log.Infof("Creating service %s", info.Name())
		errC := cp.Copy(filepath.Join(dir, info.Name()), filepath.Join("/etc/systemd/system/", info.Name()))
		if errC != nil {
			return errC
		}
		log.Infof("Enabling service %s", info.Name())
		_, errC = ops.SystemctlAction("enable", info.Name())
		return errC
	})
	return err
}
