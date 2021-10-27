// Copyright (c) 2020, 2021, Oracle and/or its affiliates.
//
// Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/

package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/mysql/ndb-operator/config/debug"
	"github.com/mysql/ndb-operator/pkg/apis/ndbcontroller/v1alpha1"
	"github.com/mysql/ndb-operator/pkg/constants"
	ndbclientset "github.com/mysql/ndb-operator/pkg/generated/clientset/versioned"
	ndbinformers "github.com/mysql/ndb-operator/pkg/generated/informers/externalversions"
	ndblisters "github.com/mysql/ndb-operator/pkg/generated/listers/ndbcontroller/v1alpha1"
	"github.com/mysql/ndb-operator/pkg/resources"
)

// ControllerContext summarizes the context in which it is running
type ControllerContext struct {
	// kubeClientset is the standard kubernetes clientset
	kubeClientset kubernetes.Interface
	ndbClientset  ndbclientset.Interface

	// runningInsideK8s is set to true if the operator is running inside a K8s cluster.
	runningInsideK8s bool
}

// NewControllerContext returns a new controller context object
func NewControllerContext(
	kubeclient kubernetes.Interface,
	ndbclient ndbclientset.Interface,
	runningInsideK8s bool,
) *ControllerContext {
	ctx := &ControllerContext{
		kubeClientset:    kubeclient,
		ndbClientset:     ndbclient,
		runningInsideK8s: runningInsideK8s,
	}

	return ctx
}

// Controller is the main controller implementation for Ndb resources
type Controller struct {
	controllerContext *ControllerContext

	// NdbCluster Lister
	ndbsLister ndblisters.NdbClusterLister

	// Controllers for various resources
	mgmdController      StatefulSetControlInterface
	ndbdController      StatefulSetControlInterface
	mysqldController    DeploymentControlInterface
	configMapController ConfigMapControlInterface
	pdbController       PodDisruptionBudgetControlInterface

	// K8s Listers
	podLister     corelisters.PodLister
	serviceLister corelisters.ServiceLister

	// Slice of InformerSynced methods for all the informers used by the controller
	informerSyncedMethods []cache.InformerSynced

	// A rate limited workqueue for queueing the NdbCluster resource
	// keys on receiving an event. The workqueue ensures that the same
	// key is not processed simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// An event recorder for recording Event resources to the Kubernetes API.
	recorder events.EventRecorder
}

// NewController returns a new Ndb controller
func NewController(
	controllerContext *ControllerContext,
	k8sSharedIndexInformer kubeinformers.SharedInformerFactory,
	ndbSharedIndexInformer ndbinformers.SharedInformerFactory) *Controller {

	// Register for all the required informers
	ndbClusterInformer := ndbSharedIndexInformer.Mysql().V1alpha1().NdbClusters()
	statefulSetInformer := k8sSharedIndexInformer.Apps().V1().StatefulSets()
	deploymentInformer := k8sSharedIndexInformer.Apps().V1().Deployments()
	podInformer := k8sSharedIndexInformer.Core().V1().Pods()
	serviceInformer := k8sSharedIndexInformer.Core().V1().Services()
	pdbInformer := k8sSharedIndexInformer.Policy().V1beta1().PodDisruptionBudgets()

	// Extract all the InformerSynced methods
	informerSyncedMethods := []cache.InformerSynced{
		ndbClusterInformer.Informer().HasSynced,
		statefulSetInformer.Informer().HasSynced,
		deploymentInformer.Informer().HasSynced,
		podInformer.Informer().HasSynced,
		serviceInformer.Informer().HasSynced,
		pdbInformer.Informer().HasSynced,
	}

	statefulSetLister := statefulSetInformer.Lister()

	controller := &Controller{
		controllerContext:     controllerContext,
		informerSyncedMethods: informerSyncedMethods,
		ndbsLister:            ndbClusterInformer.Lister(),
		serviceLister:         serviceInformer.Lister(),
		podLister:             podInformer.Lister(),
		configMapController:   NewConfigMapControl(controllerContext.kubeClientset),
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Ndbs"),
		recorder:              newEventRecorder(controllerContext.kubeClientset),

		mgmdController: NewNdbNodesStatefulSetControlInterface(
			controllerContext.kubeClientset, statefulSetLister, resources.NewMgmdStatefulSet()),
		ndbdController: NewNdbNodesStatefulSetControlInterface(
			controllerContext.kubeClientset, statefulSetLister, resources.NewNdbdStatefulSet()),
		mysqldController: NewMySQLDeploymentController(
			controllerContext.kubeClientset, deploymentInformer.Lister()),

		pdbController: NewPodDisruptionBudgetControl(
			controllerContext.kubeClientset, pdbInformer.Lister()),
	}

	klog.Info("Setting up event handlers")
	// Set up event handler for NdbCluster resource changes
	ndbClusterInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{

		AddFunc: func(obj interface{}) {
			ndb := obj.(*v1alpha1.NdbCluster)
			ndbKey := getNdbClusterKey(ndb)
			klog.Infof("New NdbCluster resource added : %q, queueing it for reconciliation", ndbKey)
			controller.workqueue.Add(ndbKey)
		},

		UpdateFunc: func(old, new interface{}) {
			oldNdb := old.(*v1alpha1.NdbCluster)
			ndbKey := getNdbClusterKey(oldNdb)

			newNdb := new.(*v1alpha1.NdbCluster)
			if oldNdb.Generation != newNdb.Generation {
				// Spec of the NdbCluster resource was updated.
				klog.Infof("Spec of the NdbCluster resource %q was updated", ndbKey)
				klog.Infof("Generation updated from %d -> %d",
					oldNdb.Generation, newNdb.Generation)
				klog.Infof("Resource version updated from %s -> %s",
					oldNdb.ResourceVersion, newNdb.ResourceVersion)
				klog.Infof("NdbCluster resource %q is added to the queue for reconciliation", ndbKey)
			} else if oldNdb.ResourceVersion != newNdb.ResourceVersion {
				// Spec was not updated but the ResourceVersion changed => Status update
				klog.Infof("Status of the NdbCluster resource '%s' was updated", ndbKey)
				klog.Infof("Resource version updated from %s -> %s",
					oldNdb.ResourceVersion, newNdb.ResourceVersion)
				klog.Info("Nothing to do as only the status was updated.")
				return
			} else {
				// NdbCluster resource was not updated and this is a resync/requeue.
				if oldNdb.Generation != oldNdb.Status.ProcessedGeneration {
					// Reconciliation is already underway. Either it is being handled by a
					// worker right now or the controller is waiting for some resource to get
					// ready, and it will continue once it is ready.
					// So, no need to add the item to the workqueue now.
					return
				}
				klog.Infof("No updates to NdbCluster resource %q, queueing it for periodic reconciliation", ndbKey)
			}
			controller.workqueue.Add(ndbKey)
		},

		DeleteFunc: func(obj interface{}) {
			// Various K8s resources created and maintained for this NdbCluster
			// resource will have proper owner resources setup. Due to that, this
			// delete will automatically be cascaded to all those resources and
			// the controller doesn't have to do anything.
			ndb := obj.(*v1alpha1.NdbCluster)
			klog.Infof("NdbCluster resource '%s' was deleted", getNdbClusterKey(ndb))
		},
	})

	// Set up event handlers for deployment resource changes
	deploymentInformer.Informer().AddEventHandlerWithResyncPeriod(

		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				// Filter out all deployments not owned by any
				// NdbCluster resources. The deployment labels
				// will have the names of their respective
				// NdbCluster owners.
				deployment := obj.(*appsv1.Deployment)
				_, clusterLabelExists := deployment.GetLabels()[constants.ClusterLabel]
				return clusterLabelExists
			},

			Handler: cache.ResourceEventHandlerFuncs{
				// When a deployment owned by a NdbCluster resource
				// is complete, add the NdbCluster resource to the
				// workqueue to start the next reconciliation loop.
				UpdateFunc: func(oldObj, newObj interface{}) {
					deployment := newObj.(*appsv1.Deployment)
					if deploymentComplete(deployment) {
						klog.Infof("Deployment %q is complete", getNamespacedName(deployment))
						controller.extractAndEnqueueNdbCluster(deployment)
					}
				},

				// When a deployment owned by a NdbCluster resource
				// is deleted, add the NdbCluster resource to the
				// workqueue to start the next reconciliation loop.
				DeleteFunc: func(obj interface{}) {
					deployment := obj.(*appsv1.Deployment)
					klog.Infof("Deployment %q is deleted", getNamespacedName(deployment))
					controller.extractAndEnqueueNdbCluster(deployment)
				},
			},
		},

		// Set resyncPeriod to 0 to ignore all re-sync events
		0,
	)

	// Set up event handlers for StatefulSet resource changes
	statefulSetInformer.Informer().AddEventHandlerWithResyncPeriod(

		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				// Filter out all StatefulSets not owned by any
				// NdbCluster resources. The StatefulSet labels
				// will have the names of their respective
				// NdbCluster owners.
				statefulset := obj.(*appsv1.StatefulSet)
				_, clusterLabelExists := statefulset.GetLabels()[constants.ClusterLabel]
				return clusterLabelExists
			},

			Handler: cache.ResourceEventHandlerFuncs{
				// When a StatefulSet owned by a NdbCluster resource
				// is ready, add the NdbCluster resource to the
				// workqueue to start the next reconciliation loop.
				UpdateFunc: func(oldObj, newObj interface{}) {
					statefulset := newObj.(*appsv1.StatefulSet)
					if statefulsetReady(statefulset) {
						klog.Infof("StatefulSet %q is ready", getNamespacedName(statefulset))
						controller.extractAndEnqueueNdbCluster(statefulset)
					}
				},
			},
		},

		// Set resyncPeriod to 0 to ignore all re-sync events
		0,
	)

	return controller
}

// extractAndEnqueueNdbCluster extracts the key of NdbCluster that owns
// the given Workload object (i.e. a deployment or a statefulset) and
// then adds it to the controller's workqueue for reconciliation.
func (c *Controller) extractAndEnqueueNdbCluster(obj metav1.Object) {
	ndbClusterName := obj.GetLabels()[constants.ClusterLabel]
	key := obj.GetNamespace() + Separator + ndbClusterName
	klog.Infof("NdbCluster resource %q is re-queued for further reconciliation", key)
	c.workqueue.Add(key)
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting Ndb controller")

	// Wait for the caches to be synced before starting workers
	if ok := cache.WaitForNamedCacheSync(
		controllerName, stopCh, c.informerSyncedMethods...); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch worker go routines to process Ndb resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(func() {
			// The workers continue processing work items
			// available in the work queue until they are shutdown
			for c.processNextWorkItem() {
			}
		}, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// processNextWorkItem reads a single work item off the
// workqueue and processes it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() (continueProcessing bool) {
	// Wait until there is a new item in the queue.
	// Get() also blocks other worker threads from
	// processing the 'item' until Done() is called on it.
	item, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// Setup defer to call Done on the item to unblock it from other workers.
	defer c.workqueue.Done(item)

	// The item is a string key of the NdbCluster
	// resource object. It is of the form 'namespace/name'.
	key, ok := item.(string)
	if !ok {
		// item was not a string. Internal error.
		// Forget the item to avoid looping on it.
		c.workqueue.Forget(item)
		klog.Error(debug.InternalError(fmt.Errorf("expected string in workqueue but got %#v", item)))
		return true
	}

	// Run the syncHandler for the extracted key.
	klog.Infof("Starting a reconciliation cycle for NdbCluster resource %q", key)
	sr := c.syncHandler(key)
	klog.Infof("Completed a reconciliation cycle for NdbCluster resource %q", key)

	if err := sr.getError(); err != nil {
		klog.Infof("Reconciliation of NdbCluster resource %q failed", key)
		// The sync failed. It will be retried.
		klog.Info("Re-queuing resource to retry reconciliation after error")
		c.workqueue.AddRateLimited(key)
		return true
	}

	// The reconciliation loop was successful. Clear rateLimiter.
	c.workqueue.Forget(item)

	// Requeue the item if necessary
	if requeue, requeueInterval := sr.requeueSync(); requeue {
		klog.Infof("NdbCluster resource %q will be re-queued for further reconciliation", key)
		c.workqueue.AddAfter(item, requeueInterval)
	}

	return true
}

func (c *Controller) newSyncContext(ndb *v1alpha1.NdbCluster) *SyncContext {
	return &SyncContext{
		mgmdController:      c.mgmdController,
		ndbdController:      c.ndbdController,
		mysqldController:    c.mysqldController,
		configMapController: c.configMapController,
		pdbController:       c.pdbController,
		ndb:                 ndb,
		controllerContext:   c.controllerContext,
		ndbsLister:          c.ndbsLister,
		podLister:           c.podLister,
		serviceLister:       c.serviceLister,
		recorder:            c.recorder,
	}
}

// syncHandler is the main reconciliation function
//   driving cluster towards desired configuration
//
// - synchronization happens in multiple steps
// - not all actions are taking in one call of syncHandler
//
// main principle:
// the desired state in Ndb CRD must be reflected in the Ndb configuration file
// and the cluster state will be adopted *to that config file first*
// before new changes from Ndb CRD are accepted
//
// Sync steps
//
// 1. ensure all resources are correctly created
// 2. ensure cluster is fully up and running and not in a degraded state
//    before rolling out any changes
// 3. drive cluster components towards the configuration previously
//    written to the configuration file
// 4. only after complete cluster is aligned with configuration file
//    new changes from Ndb CRD are written to a new version of the config file
// 5. update status of the CRD
func (c *Controller) syncHandler(key string) syncResult {

	klog.Infof("Sync handler: %s", key)

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		// no need to retry as this is a permanent error
		return finishProcessing()
	}

	// Get the NdbCluster resource with this namespace/name
	ndbOrg, err := c.ndbsLister.NdbClusters(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Stop processing if the NdbCluster resource no longer exists
			klog.Infof("NdbCluster resource %q does not exist anymore", key)
			return finishProcessing()
		}

		klog.Infof("Failed to retrieve NdbCluster resource %q", key)
		return errorWhileProcessing(err)
	}

	// take a copy and process that for the update at the end
	// make all changes to the copy only and patch the original in the end
	ndb := ndbOrg.DeepCopy()

	syncContext := c.newSyncContext(ndb)

	return syncContext.sync(context.TODO())
}
