package seedcreator

import (
	"context"
	"errors"
	"fmt"

	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	v1 "github.com/openshift/api/config/v1"
	cp "github.com/otiai10/copy"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/types"
	runtime "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	varFolder = "/var"
)

// containerFileContent is the Dockerfile content for the IBU seed image
const containerFileContent = `
FROM scratch
COPY . /
`

// SeedCreator TODO: move params to Options
type SeedCreator struct {
	client               runtime.Client
	log                  *logrus.Logger
	ops                  ops.Ops
	ostreeClient         *ostree.Client
	backupDir            string
	kubeconfig           string
	containerRegistry    string
	authFile             string
	recertContainerImage string
	manifestClient       *clusterinfo.InfoClient
}

// NewSeedCreator is a constructor function for SeedCreator
func NewSeedCreator(client runtime.Client, log *logrus.Logger, ops ops.Ops, ostreeClient *ostree.Client, backupDir,
	kubeconfig, containerRegistry, authFile, recertContainerImage string) *SeedCreator {

	return &SeedCreator{
		client:               client,
		log:                  log,
		ops:                  ops,
		ostreeClient:         ostreeClient,
		backupDir:            backupDir,
		kubeconfig:           kubeconfig,
		containerRegistry:    containerRegistry,
		authFile:             authFile,
		recertContainerImage: recertContainerImage,
		manifestClient:       clusterinfo.NewClusterInfoClient(client),
	}
}

// CreateSeedImage comprises the Imager workflow for creating a single OCI seed image
func (s *SeedCreator) CreateSeedImage() error {
	s.log.Println("Creating seed image")

	// create backup dir
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return err
	}

	if err := s.createContainerList(); err != nil {
		return err
	}

	if err := s.gatherClusterInfo(context.TODO()); err != nil {
		return err
	}

	if err := s.stopServices(); err != nil {
		return err
	}

	if err := s.backupVar(); err != nil {
		return err
	}

	if err := s.backupEtc(); err != nil {
		return err
	}

	if err := s.backupOstree(); err != nil {
		return err
	}

	if err := s.backupRPMOstree(); err != nil {
		return err
	}

	if err := s.backupMCOConfig(); err != nil {
		return err
	}

	if err := s.createAndPushSeedImage(); err != nil {
		return err
	}

	return nil
}

func (s *SeedCreator) gatherClusterInfo(ctx context.Context) error {
	// TODO: remove after removing usage of clusterversion.json
	clusterVersion := &v1.ClusterVersion{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return err
	}
	clusterManifest, err := s.manifestClient.CreateClusterInfo(ctx)
	if err != nil {
		return err
	}

	s.log.Println("Creating manifest.json")
	if err := utils.WriteToFile(clusterManifest, path.Join(s.backupDir, "manifest.json")); err != nil {
		return err
	}

	// TODO: remove when we will drop it from preparation script
	s.log.Println("Creating clusterversion.json")
	if err := utils.WriteToFile(clusterVersion, path.Join(s.backupDir, "clusterversion.json")); err != nil {
		return err
	}

	return s.renderInstallationEnvFile(s.recertContainerImage,
		fmt.Sprintf("%s.%s", clusterManifest.ClusterName, clusterManifest.Domain))
}

// TODO: split function per operation
func (s *SeedCreator) createContainerList() error {
	s.log.Println("Saving list of running containers and catalogsources.")

	// Check if the file /var/tmp/container_list.done does not exist
	if _, err := os.Stat("/var/tmp/container_list.done"); os.IsNotExist(err) {
		// Execute 'crictl images -o json' command, parse the JSON output and extract image references using 'jq'
		s.log.Println("Save list of running containers")
		args := []string{"images", "-o", "json", "|", "jq", "-r", "'.images[] | .repoDigests[], .repoTags[]'",
			">", s.backupDir + "/containers.list"}

		_, err = s.ops.RunBashInHostNamespace("crictl", args...)
		if err != nil {
			return err
		}

		// Execute 'oc get catalogsource' command, parse the JSON output and extract image references using 'jq'
		s.log.Println("Save catalog source images")
		_, err = s.ops.RunBashInHostNamespace(
			"oc", append([]string{"get", "catalogsource", "-A", "-o", "json", "--kubeconfig",
				s.kubeconfig, "|", "jq", "-r", "'.items[].spec.image'"}, ">", s.backupDir+"/catalogimages.list")...)
		if err != nil {
			return err
		}

		// Create the file /var/tmp/container_list.done
		_, err = os.Create("/var/tmp/container_list.done")
		if err != nil {
			return err
		}

		s.log.Println("List of containers, catalogsources, and clusterversion saved successfully.")
	} else {
		s.log.Println("Skipping list of containers, catalogsources, and clusterversion already exists.")
	}
	return nil
}

func (s *SeedCreator) stopServices() error {
	s.log.Println("Stop kubelet service")
	_, err := s.ops.SystemctlAction("stop", "kubelet.service")
	if err != nil {
		return err
	}

	s.log.Println("Disabling kubelet service")
	_, err = s.ops.SystemctlAction("disable", "kubelet.service")
	if err != nil {
		return err
	}

	s.log.Println("Stopping containers and CRI-O runtime.")
	crioSystemdStatus, err := s.ops.SystemctlAction("is-active", "crio")
	var exitErr *exec.ExitError
	// If ExitCode is 3, the command succeeded and told us that crio is down
	if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() != 3 {
		return err
	}
	s.log.Println("crio status is", crioSystemdStatus)
	if crioSystemdStatus == "active" {

		// CRI-O is active, so stop running containers
		s.log.Println("Stop running containers")
		args := []string{"ps", "-q", "|", "xargs", "--no-run-if-empty", "--max-args", "1", "--max-procs", "10", "crictl", "stop", "--timeout", "5"}
		_, err = s.ops.RunBashInHostNamespace("crictl", args...)
		if err != nil {
			return err
		}

		// Execute a D-Bus call to stop the CRI-O runtime
		s.log.Debug("Stopping CRI-O engine")
		_, err = s.ops.SystemctlAction("stop", "crio.service")
		if err != nil {
			return err
		}
		s.log.Println("Running containers and CRI-O engine stopped successfully.")
	} else {
		s.log.Println("Skipping running containers and CRI-O engine already stopped.")
	}

	return nil
}

func (s *SeedCreator) backupVar() error {
	// Check if the backup file for /var doesn't exist
	varTarFile := path.Join(s.backupDir, "var.tgz")
	_, err := os.Stat(varTarFile)
	if err == nil || !os.IsNotExist(err) {
		return err
	}

	// Define the 'exclude' patterns
	excludePatterns := []string{
		"/var/tmp/*",
		"/var/lib/log/*",
		"/var/log/*",
		"/var/lib/containers/*",
		"/var/lib/kubelet/pods/*",
		"/var/lib/cni/bin/*",
	}

	// Build the tar command
	tarArgs := []string{"czf", varTarFile}
	for _, pattern := range excludePatterns {
		// We're handling the excluded patterns in bash, we need to single quote them to prevent expansion
		tarArgs = append(tarArgs, "--exclude", fmt.Sprintf("'%s'", pattern))
	}
	tarArgs = append(tarArgs, "--selinux", varFolder)

	// Run the tar command
	_, err = s.ops.RunBashInHostNamespace("tar", tarArgs...)
	if err != nil {
		return err
	}

	s.log.Infof("Backup of %s created successfully.", varFolder)
	return nil
}

func (s *SeedCreator) backupEtc() error {
	s.log.Println("Backing up /etc")
	_, err := os.Stat(path.Join(s.backupDir, "etc.tgz"))
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}

	// save old ip as it is required for installation process by installation-configuration.sh
	err = cp.Copy("/var/run/nodeip-configuration/primary-ip", "/etc/default/seed-ip")
	if err != nil {
		return err
	}

	// Execute 'ostree admin config-diff' command and backup etc.deletions
	args := []string{"admin", "config-diff", "|", "awk", `'$1 == "D" {print "/etc/" $2}'`, ">",
		path.Join(s.backupDir, "/etc.deletions")}
	_, err = s.ops.RunBashInHostNamespace("ostree", args...)
	if err != nil {
		return err
	}

	args = []string{"admin", "config-diff", "|", "awk", `'$1 != "D" {print "/etc/" $2}'`, "|", "xargs", "tar", "czf",
		path.Join(s.backupDir + "/etc.tgz"), "--selinux"}
	_, err = s.ops.RunBashInHostNamespace("ostree", args...)
	if err != nil {
		return err
	}
	s.log.Println("Backup of /etc created successfully.")

	return nil
}

func (s *SeedCreator) backupOstree() error {
	// Check if the backup file for ostree doesn't exist
	s.log.Println("Backing up ostree")
	ostreeTar := s.backupDir + "/ostree.tgz"
	_, err := os.Stat(ostreeTar)
	if err == nil || !os.IsNotExist(err) {
		return err
	}
	// Execute 'tar' command and backup /etc
	_, err = s.ops.RunBashInHostNamespace(
		"tar", []string{"czf", ostreeTar, "--selinux", "-C", "/ostree/repo", "."}...)

	return err
}

func (s *SeedCreator) backupRPMOstree() error {
	// Check if the backup file for rpm-ostree doesn't exist
	rpmJSON := s.backupDir + "/rpm-ostree.json"
	_, err := os.Stat(rpmJSON)
	if err == nil || !os.IsNotExist(err) {
		return err
	}
	_, err = s.ops.RunBashInHostNamespace(
		"rpm-ostree", append([]string{"status", "-v", "--json"}, ">", rpmJSON)...)
	log.Println("Backup of rpm-ostree.json created successfully.")
	return err
}

func (s *SeedCreator) backupMCOConfig() error {
	// Check if the backup file for mco-currentconfig doesn't exist
	mcoJSON := s.backupDir + "/mco-currentconfig.json"
	_, err := os.Stat(mcoJSON)
	if err == nil || !os.IsNotExist(err) {
		return err
	}
	_, err = s.ops.RunBashInHostNamespace(
		"cp", "/etc/machine-config-daemon/currentconfig", mcoJSON)
	log.Println("Backup of mco-currentconfig created successfully.")
	return err
}

// Building and pushing OCI image
func (s *SeedCreator) createAndPushSeedImage() error {
	s.log.Println("Build and push OCI image to", s.containerRegistry)
	s.log.Debug(s.ostreeClient.RpmOstreeVersion()) // If verbose, also dump out current rpm-ostree version available

	// Get the current status of rpm-ostree daemon in the host
	statusRpmOstree, err := s.ostreeClient.QueryStatus()
	if err != nil {
		return fmt.Errorf("failed to query ostree status: %w", err)
	}
	if err := s.backupOstreeOrigin(statusRpmOstree); err != nil {
		return err
	}

	// Create a temporary file for the Dockerfile content
	tmpfile, err := os.CreateTemp("/var/tmp", "dockerfile-")
	if err != nil {
		return fmt.Errorf("error creating temporary file: %w", err)
	}
	defer os.Remove(tmpfile.Name()) // Clean up the temporary file

	// Write the content to the temporary file
	_, err = tmpfile.WriteString(containerFileContent)
	if err != nil {
		return fmt.Errorf("error writing to temporary file: %w", err)
	}
	_ = tmpfile.Close() // Close the temporary file

	// Build the single OCI image (note: We could include --squash-all option, as well)
	_, err = s.ops.RunInHostNamespace(
		"podman", []string{"build", "-f", tmpfile.Name(), "-t", s.containerRegistry, s.backupDir}...)
	if err != nil {
		return fmt.Errorf("failed to build seed image: %w", err)
	}

	// Push the created OCI image to user's repository
	_, err = s.ops.RunInHostNamespace(
		"podman", []string{"push", "--authfile", s.authFile, s.containerRegistry}...)
	if err != nil {
		return fmt.Errorf("failed to push seed image: %w", err)
	}
	return nil
}

func (s *SeedCreator) backupOstreeOrigin(statusRpmOstree *ostree.Status) error {

	// Get OSName for booted ostree deployment
	bootedOSName := statusRpmOstree.Deployments[0].OSName
	// Get ID for booted ostree deployment
	bootedID := statusRpmOstree.Deployments[0].ID
	// Get SHA for booted ostree deployment
	bootedDeployment := strings.Split(bootedID, "-")[1]

	// Check if the backup file for .origin doesn't exist
	originFileName := fmt.Sprintf("%s/ostree-%s.origin", s.backupDir, bootedDeployment)
	_, err := os.Stat(originFileName)
	if err == nil || !os.IsNotExist(err) {
		return err
	}
	// Execute 'copy' command and backup .origin file
	_, err = s.ops.RunInHostNamespace(
		"cp", []string{"/ostree/deploy/" + bootedOSName + "/deploy/" + bootedDeployment + ".origin", originFileName}...)
	if err != nil {
		return err
	}
	log.Println("Backup of .origin created successfully.")
	return nil
}

func (s *SeedCreator) renderInstallationEnvFile(recertContainerImage, seedFullDomain string) error {
	// Define source and destination file paths
	srcFile := "installation_configuration_files/conf/installation-configuration.env"
	destFile := "/etc/systemd/system/installation-configuration.env"
	// Prepare variable substitutions for template files
	substitutions := map[string]any{
		"RecertContainerImage": recertContainerImage,
		"SeedFullDomain":       seedFullDomain,
	}

	return utils.RenderTemplateFile(srcFile, substitutions, destFile, 0o644)
}
