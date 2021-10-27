// Copyright (c) 2020, 2021, Oracle and/or its affiliates.
//
// Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/

// Tool to run end-to-end tests using Ginkgo and Kind/Minikube

//go:build ignore
// +build ignore

package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	podutils "github.com/mysql/ndb-operator/e2e-tests/utils/pods"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Command line options
var options struct {
	useKind        bool
	kubeconfig     string
	inCluster      bool
	kindK8sVersion string
	suites         string
}

// K8s image used by KinD to bring up cluster
// https://github.com/kubernetes-sigs/kind/releases
var kindK8sNodeImages = map[string]string{
	"1.19": "kindest/node:v1.19.11@sha256:07db187ae84b4b7de440a73886f008cf903fcf5764ba8106a9fd5243d6f32729",
	"1.20": "kindest/node:v1.20.7@sha256:cbeaf907fc78ac97ce7b625e4bf0de16e3ea725daf6b04f930bd14c67c671ff9",
	"1.21": "kindest/node:v1.21.1@sha256:69860bda5563ac81e3c0057d654b5253219618a22ec3a346306239bba8cfa1a6",
	"1.22": "kindest/node:v1.22.0@sha256:b8bda84bb3a190e6e028b1760d277454a72267a5454b57db34437c34a588d047",
}

var (
	kindCmd = []string{"go", "run", "sigs.k8s.io/kind"}
)

// provider is an interface for the k8s cluster providers
type provider interface {
	// setupK8sCluster sets up the provider specific cluster.
	// It returns true if it succeeded in its attempt.
	setupK8sCluster(t *testRunner) bool
	getKubeConfig() string
	teardownK8sCluster(t *testRunner)
	runGinkgoTestsInsideCluster(t *testRunner) bool
}

// providerDefaults defines the common fields and
// methods to be used by the other providers
type providerDefaults struct {
	// kubeconfig is the kubeconfig of the cluster
	kubeconfig string
	// kubernetes clientset
	clientset kubernetes.Interface
}

// getKubeConfig returns the Kubeconfig to connect to the cluster
func (p *providerDefaults) getKubeConfig() string {
	return p.kubeconfig
}

// initClientsetFromKubeconfig creates a kubernetes clientset from the given kubeconfig
func (p *providerDefaults) initClientsetFromKubeconfig() (success bool) {
	config, err := clientcmd.BuildConfigFromFlags("", p.kubeconfig)
	if err != nil {
		log.Printf(" ❌ Error building config from kubeconfig '%s': %s", p.kubeconfig, err)
		return false
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf(" ❌ Error creating new kubernetes clientset: %s", err)
		return false
	}

	// success
	p.clientset = clientset
	return true
}

// waitForPodToStart waits until all the containers in the given pod are started
func (p *providerDefaults) waitForPodToStart(namespace, name string) (podStarted bool) {
	if err := podutils.WaitForPodToStart(p.clientset, namespace, name); err != nil {
		log.Printf(" ❌ Error waiting for the e2e-tests pod to start : %s", err)
		return false
	}

	return true
}

// waitForPodToTerminate waits until all the containers in the given pod are terminated
func (p *providerDefaults) waitForPodToTerminate(namespace, name string) (podTerminated bool) {
	if err := podutils.WaitForPodToTerminate(p.clientset, namespace, name); err != nil {
		log.Printf("❌ Error waiting for the e2e-tests pod to terminate : %s", err)
		return false
	}

	return true
}

// local implements a provider to connect to an existing K8s cluster
type local struct {
	providerDefaults
}

// newLocalProvider returns a new local provider
func newLocalProvider() *local {
	log.Println("🔧 Configuring tests to run on an existing cluster")
	return &local{}
}

// setupK8sCluster just connects to the Kubernetes Server using the
// kubeconfig passed as the cluster is expected to be running already.
func (l *local) setupK8sCluster(*testRunner) (success bool) {
	// Connect to the K8s Cluster using the kubeconfig
	if len(options.kubeconfig) > 0 {
		l.kubeconfig = options.kubeconfig
		if !l.initClientsetFromKubeconfig() {
			return false
		}

		// Retrieve version and verify
		var k8sVersion *version.Info
		var err error
		if k8sVersion, err = l.clientset.Discovery().ServerVersion(); err != nil {
			log.Printf("❌ Error finding out the version of the K8s cluster : %s", err)
			return false
		}

		log.Printf("👍 Successfully validated kubeconfig and connected to the Kubernetes Server.\n"+
			"Kubernetes Server version : %s", k8sVersion.String())
		return true
	}

	log.Println("⚠️  Please pass a valid kubeconfig")
	return false
}

// teardownK8sCluster is a no-op for local provider
// TODO: Maybe verify all the test resources are cleaned up here?
func (l *local) teardownK8sCluster(*testRunner) {}

// runGinkgoTestsInsideCluster is a no-op for local provider
func (l *local) runGinkgoTestsInsideCluster(*testRunner) bool {
	log.Fatal("❌ Running ginkgo tests inside cluster not supported for local provider.")
	return false
}

// kind implements a provider to control k8s clusters in KinD
type kind struct {
	providerDefaults
	// cluster name
	clusterName string
}

// newKindProvider returns a new kind provider
func newKindProvider() *kind {
	log.Println("🔧 Configuring tests to run on a KinD cluster")
	return &kind{}
}

// setupK8sCluster starts a K8s cluster using KinD
func (k *kind) setupK8sCluster(t *testRunner) bool {
	// Verify that docker is running
	if !t.execCommand([]string{"docker", "info"}, "docker info", true, false) {
		// Docker not running. Exit here as there is nothing to cleanup.
		log.Fatal("⚠️  Please ensure that docker daemon is running and accessible.")
	}
	log.Println("🐳 Docker daemon detected and accessible!")

	if !k.createKindCluster(t) {
		return false
	}

	// Load the operator docker image into cluster nodes
	if !k.loadImageToKindCluster("mysql/ndb-operator:latest", t) {
		return false
	}

	if options.inCluster {
		// Load e2e-tests docker image into cluster nodes
		if !k.loadImageToKindCluster("e2e-tests:latest", t) {
			return false
		}
	}

	// init clientset to the k8s cluster
	if !k.initClientsetFromKubeconfig() {
		return false
	}

	return true
}

// createKindCluster creates a kind cluster 'ndb-e2e-test'
// It returns true on success.
func (k *kind) createKindCluster(t *testRunner) bool {
	// custom kubeconfig
	k.kubeconfig = filepath.Join(t.testDir, "_artifacts", ".kubeconfig")
	// kind cluster name
	k.clusterName = "ndb-e2e-test"
	// KinD k8s image used to run tests
	kindK8sNodeImage := kindK8sNodeImages[options.kindK8sVersion]
	// Build KinD command and args
	kindCreateCluster := append(kindCmd,
		// create cluster
		"create", "cluster",
		// cluster name
		"--name="+k.clusterName,
		// kubeconfig
		"--kubeconfig="+k.kubeconfig,
		// kind k8s node image to be used
		"--image="+kindK8sNodeImage,
		// cluster configuration
		"--config="+filepath.Join(t.testDir, "_config", "kind-3-node-cluster.yaml"),
	)

	// Run the command to create a cluster
	if !t.execCommand(kindCreateCluster, "kind create cluster", false, true) {
		log.Println("❌ Failed to create cluster using KinD")
		return false
	}
	log.Println("✅ Successfully started a KinD cluster")
	return true
}

// loadImageToKindCluster loads docker image to kind cluster
// It returns true on success.
func (k *kind) loadImageToKindCluster(image string, t *testRunner) bool {
	kindLoadImage := append(kindCmd,
		// load docker-image
		"load", "docker-image",
		// image name
		image,
		// cluster name
		"--name="+k.clusterName,
	)
	// Run the command to load docker image
	if !t.execCommand(kindLoadImage, "kind load docker-image", false, true) {
		log.Printf("❌ Failed to load '%s' image into KinD cluster", image)
		return false
	}
	log.Printf("✅ Successfully loaded '%s' image into the KinD cluster", image)
	return true
}

// runGinkgoTestInsideCluster runs all tests as a pod inside kind cluster
// It returns true if tests run successfully
func (k *kind) runGinkgoTestsInsideCluster(t *testRunner) bool {
	e2eArtifacts := filepath.Join(t.testDir, "_config", "k8s-deployment")
	// Build kubectl command to create kubernetes resources to run e2e-tests,
	// using e2e artifacts
	kubectlCommand := getKubectlCommand(k)
	createE2eTestK8sResources := append(kubectlCommand,
		"apply", "-f",
		// e2e-tests artifacts
		e2eArtifacts,
	)
	if !t.execCommand(createE2eTestK8sResources, "kubectl apply", false, true) {
		log.Println("❌ Failed to create kubernetes resources using e2e-test artifacts")
		return false
	}
	log.Println("✅ Successfully created the required RBACs for the e2e tests pod")

	// 'e2e-tests/suites' is the path to testsuite directory, in e2e-tests image
	// Append ginkgo command to run specific test suites
	ginkgoTestCmd := t.getGinkgoTestCommand("e2e-tests/suites")

	// create e2e-tests-pod
	e2eTestPod := k.createE2eTestsPod(ginkgoTestCmd)
	_, err := k.clientset.CoreV1().Pods(e2eTestPod.Namespace).Create(context.TODO(), e2eTestPod, metav1.CreateOptions{})
	if err != nil {
		log.Printf("❌ Error creating e2e-tests pod: %s", err)
	}
	log.Println("✅ Successfully created the the e2e tests pod")

	// Wait for the pods to start
	if !k.waitForPodToStart(e2eTestPod.Namespace, e2eTestPod.Name) {
		dumpPodInfo(t, e2eTestPod.Namespace, e2eTestPod.Name)
		return false
	}
	log.Println("🏃 The e2e tests pod has started running")
	log.Println("📃 Redirecting pod logs to console...")

	// build kubectl command to print e2e-tests pod logs onto console
	e2eTestPodLogs := append(kubectlCommand,
		"logs", "-f",
		// e2e tests pod
		"--namespace="+e2eTestPod.Namespace,
		e2eTestPod.Name,
	)
	if !t.execCommand(e2eTestPodLogs, "kubectl logs", false, true) {
		log.Println("❌ Failed to get e2e-tests pod logs.")
		dumpPodInfo(t, e2eTestPod.Namespace, e2eTestPod.Name)
		return false
	}

	// Wait for the pod to terminate
	if !k.waitForPodToTerminate(e2eTestPod.Namespace, e2eTestPod.Name) {
		dumpPodInfo(t, e2eTestPod.Namespace, e2eTestPod.Name)
		return false
	}

	if !podutils.HasPodSucceeded(k.clientset, e2eTestPod.Namespace, e2eTestPod.Name) {
		dumpPodInfo(t, e2eTestPod.Namespace, e2eTestPod.Name)
		log.Println("❌ There are test failures!")
		return false
	}
	log.Println("😊 All tests ran successfully!")
	return true
}

// createE2eTestsPod creates a new e2e-tests pod and returns the Pod object.
// So created pod will run specific test suites.
func (k *kind) createE2eTestsPod(cmd []string) *v1.Pod {
	// e2e-tests container to be run inside the pod
	e2eTestsContainer := v1.Container{
		// Container name, image used and image pull policy
		Name:            "e2e-tests-container",
		Image:           "e2e-tests",
		ImagePullPolicy: v1.PullNever,
		// Command that will be run by the container
		Command: cmd,
	}

	e2eTestsPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// Pod name and namespace
			Name:      "e2e-tests-pod",
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{e2eTestsContainer},
			// Service account with necessary RBAC authorization
			ServiceAccountName: "e2e-tests-service-account",
			RestartPolicy:      v1.RestartPolicyNever,
		},
	}

	return e2eTestsPod
}

// teardownK8sCluster deletes the KinD cluster
func (k *kind) teardownK8sCluster(t *testRunner) {
	// Build KinD command and args
	kindCmdAndArgs := append(kindCmd,
		// create cluster
		"delete", "cluster",
		// cluster name
		"--name="+k.clusterName,
		// kubeconfig
		"--kubeconfig="+k.kubeconfig,
	)

	// Run the command
	t.execCommand(kindCmdAndArgs, "kind delete cluster", false, true)
}

// getKubectlCommand builds the kubectl command
// with necessary kubeconfig and context
func getKubectlCommand(pr provider) []string {
	kubectlCmd := []string{"kubectl"}

	switch p := pr.(type) {
	case *local:
		// Provider is local
		kubectlCmd = append(kubectlCmd,
			"--kubeconfig="+p.kubeconfig)
	case *kind:
		// KinD provider
		kubectlCmd = append(kubectlCmd,
			"--kubeconfig="+p.kubeconfig,
			// context that kubectl runs against
			"--context=kind-"+p.clusterName)
	}

	return kubectlCmd
}

// dumpPodInfo dumps the pod details using kubectl describe
func dumpPodInfo(t *testRunner, namespace, name string) {
	kubectlCmd := getKubectlCommand(t.p)
	kubectlCmd = append(kubectlCmd,
		"--namespace="+namespace,
		"describe", "pods", name)

	// ignore command exit value
	t.execCommand(kubectlCmd, "kubectl describe", false, true)
}

// testRunner is the struct used to run the e2e test
type testRunner struct {
	// testDir is the absolute path of e2e test directory
	testDir string
	// p is the provider used to execute the test
	p provider
	// sigMutex is the mutex used to protect process
	// and ignoreSignals access across goroutines
	sigMutex sync.Mutex
	// process started by the execCommand
	// used to send signals when it is running
	// Access should be protected by sigMutex
	process *os.Process
	// passSignals enables passing signals to the
	// process started by the testRunner
	// Access should be protected by sigMutex
	passSignals bool
	// runDone is the channel used to signal that
	// the run method has completed. Used by
	// signalHandler to stop listening for signals
	runDone chan bool
}

// init sets up the testRunner
func (t *testRunner) init() {
	// Update log to print only line numbers
	log.SetFlags(log.Lshortfile)

	// Deduce test root directory
	var _, currentFilePath, _, _ = runtime.Caller(0)
	t.testDir = filepath.Dir(currentFilePath)
}

func (t *testRunner) startSignalHandler() {
	// Create the runDone channel
	t.runDone = make(chan bool)
	// Start a go routine to handle signals
	go func() {
		// Create a channel to receive any signal
		sigs := make(chan os.Signal, 2)
		signal.Notify(sigs)

		// Handle all the signals as follows
		// - If a process is running and ignoreSignals
		//   is enabled, send the signal to the process.
		// - If a process is running and ignoreSignals
		//   is disabled, ignore the signal
		// - If no process is running, handle it appropriately
		// - Return when the main return signals done
		for {
			select {
			case sig := <-sigs:
				t.sigMutex.Lock()
				if t.process != nil {
					if t.passSignals {
						// Pass the signal to the process
						_ = t.process.Signal(sig)
					} // else it is ignored
				} else {
					// no process running - handle it
					if sig == syscall.SIGINT ||
						sig == syscall.SIGQUIT ||
						sig == syscall.SIGTSTP {
						// Test is being aborted
						// teardown the cluster and exit
						t.p.teardownK8sCluster(t)
						log.Fatalf("⚠️  Test was aborted!")
					}
				}
				t.sigMutex.Unlock()
			case <-t.runDone:
				// run method has completed - stop signal handler
				return
			}
		}
	}()
}

// stopSignalHandler stops the signal handler
func (t *testRunner) stopSignalHandler() {
	t.runDone <- true
}

// execCommand executes the command along with its arguments
// passed through commandAndArgs slice. commandName is a log
// friendly name of the command to be used in the logs. The
// command output can be suppressed by enabling the quiet
// parameter. passSignals should be set to true if the
// signals received by the testRunner needs be passed to the
// process started by this function.
// It returns true id command got executed successfully
// and false otherwise.
func (t *testRunner) execCommand(
	commandAndArgs []string, commandName string, quiet bool, passSignals bool) bool {
	// Create cmd struct
	cmd := exec.Command(commandAndArgs[0], commandAndArgs[1:]...)

	// Map the stout and stderr if not quiet
	if !quiet {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	// Protect cmd.Start() by sigMutex to avoid signal
	// handler wrongly processing the signals itself
	// after the process has been started
	t.sigMutex.Lock()

	// Start the command
	if err := cmd.Start(); err != nil {
		log.Printf("❌ Starting '%s' failed : %s", commandName, err)
		return false
	}

	// Setup variables to be used the signal handler
	// before unlocking the sigMutex
	t.process = cmd.Process
	t.passSignals = passSignals
	t.sigMutex.Unlock()
	defer func() {
		// Reset the signal handler variables before returning
		t.sigMutex.Lock()
		t.process = nil
		t.sigMutex.Unlock()
	}()

	// Wait for the process to complete
	if err := cmd.Wait(); err != nil {
		log.Printf("❌ Running '%s' failed : %s", commandName, err)
		return false
	}

	return true
}

// getGinkgoTestCommand builds and returns the ginkgo test command
// that will be executed by the testRunner.
func (t *testRunner) getGinkgoTestCommand(suiteDir string) []string {
	// The ginkgo test command
	ginkgoTestCmd := []string{
		"go", "run", "github.com/onsi/ginkgo/ginkgo",
		"-r",         // recursively run all suites in the given directory
		"-keepGoing", // keep running all test suites even if one fails
	}

	if options.suites == "" {
		// Run all test suites
		ginkgoTestCmd = append(ginkgoTestCmd, suiteDir)
	} else {
		// Append ginkgo test suite directories, to run specific testsuites
		for _, suite := range strings.Split(options.suites, ",") {
			ginkgoTestCmd = append(ginkgoTestCmd,
				filepath.Join(suiteDir, suite))
		}
	}

	return ginkgoTestCmd
}

// runGinkgoTests runs all the tests using ginkgo
// It returns true if all tests ran successfully
func (t *testRunner) runGinkgoTests() bool {
	suiteDir := filepath.Join(t.testDir, "suites")
	// get ginkgo command to run specific test suites
	ginkgoTestCmd := t.getGinkgoTestCommand(suiteDir)

	// Append arguments to pass to the testcases
	ginkgoTestCmd = append(ginkgoTestCmd, "--", "--kubeconfig="+t.p.getKubeConfig())

	// Execute it
	log.Println("🔨 Running tests using ginkgo : " + strings.Join(ginkgoTestCmd, " "))
	if t.execCommand(ginkgoTestCmd, "ginkgo", false, true) {
		log.Println("😊 All tests ran successfully!")
		return true
	}

	log.Println("❌ There are test failures!")
	return false
}

// run executes the complete e2e test
// It returns true if all tests run successfully
func (t *testRunner) run() bool {
	// Choose a provider
	var p provider
	if options.useKind {
		p = newKindProvider()
	} else {
		p = newLocalProvider()
	}
	// store it in testRunner
	t.p = p

	// Start signal handler
	t.startSignalHandler()

	// setup defer to teardown cluster if
	// the method returns after this point
	defer func() {
		p.teardownK8sCluster(t)
		t.stopSignalHandler()
	}()

	// Set up the K8s cluster
	if !p.setupK8sCluster(t) {
		// Failed to set up cluster.
		return false
	}

	// Run the tests
	if options.inCluster {
		// run tests as K8s pods inside kind cluster
		log.Printf("🔨 Running tests from inside the KinD cluster")
		return p.runGinkgoTestsInsideCluster(t)
	} else {
		// run tests as external go application
		return t.runGinkgoTests()
	}
}

func init() {

	flag.BoolVar(&options.useKind, "use-kind", false,
		"Use KinD to run the e2e tests.\nBy default, this is disabled and the tests will be run in an existing K8s cluster.")

	// use kubeconfig at $HOME/.kube/config as the default
	defaultKubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	flag.StringVar(&options.kubeconfig, "kubeconfig", defaultKubeconfig,
		"Kubeconfig of the existing K8s cluster to run tests on.\nThis will not be used if '--use-kind' is enabled.")

	flag.BoolVar(&options.inCluster, "in-cluster", false,
		"Run tests as K8s pod inside cluster.")

	// use v1.21 as default kind k8s version
	flag.StringVar(&options.kindK8sVersion, "kind-k8s-version", "1.21",
		"Kind k8s version used to run tests. Example usage: --kind-k8s-version=1.20")

	// test suites to be run.
	flag.StringVar(&options.suites, "suites", "",
		"Test suites that needs to be run. Example usage: --suites=mysql,basic")
}

// validatesCommandlineArgs validates if command line arguments,
// have been provided acceptable values.
// It exits program execution with status code 1 if validation fails.
func validateCommandlineArgs() {
	// validate supported kind K8s version
	_, exists := kindK8sNodeImages[options.kindK8sVersion]
	if !exists {
		var supportedKindK8sVersions string
		for key := range kindK8sNodeImages {
			supportedKindK8sVersions += ", " + key
		}
		log.Printf("❌ KinD version %s not supported. Supported KinD versions are%s", options.kindK8sVersion, supportedKindK8sVersions)
		os.Exit(1)
	}

	// validate if test suites exist
	// Deduce test root directory
	var _, currentFilePath, _, _ = runtime.Caller(0)
	testDir := filepath.Dir(currentFilePath)
	suitesDir := filepath.Join(testDir, "suites")
	for _, suite := range strings.Split(options.suites, ",") {
		suite = filepath.Join(suitesDir, suite)
		if _, err := os.Stat(suite); os.IsNotExist(err) {
			log.Printf("❌ Test suite %s doesn't exist.", suite)
			log.Printf("Please find available test suites in %s.", suitesDir)
			os.Exit(1)
		}
	}
}

func main() {
	flag.Parse()
	validateCommandlineArgs()
	t := testRunner{}
	t.init()
	if !t.run() {
		// exit with status code 1 on cluster setup failure or test failures
		os.Exit(1)
	}
}
