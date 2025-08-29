package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	resourcev1 "k8s.io/api/resource/v1"
)

// Reconciler is the interface that a specific controller implementation must satisfy.
type Reconciler interface {
	// IsDeviceClassRelevant checks if this controller is responsible for the given DeviceClass.
	IsDeviceClassRelevant(deviceClass *resourcev1.DeviceClass) bool

	// Reconcile is called when a ResourceClaim is added or updated. It should create
	// the necessary ResourceSlice.
	Reconcile(ctx context.Context, claim *resourcev1.ResourceClaim) (*resourcev1.ResourceSlice, error)

	// Delete is called when a ResourceClaim is deleted. It should clean up any
	// resources that were created for the claim, like the ResourceSlice.
	Delete(ctx context.Context, claim *resourcev1.ResourceClaim) error
}

// Controller manages the lifecycle of the Kubernetes controller.
type Controller struct {
	controllerName string
	kubeClient     kubernetes.Interface
	informer       cache.SharedIndexInformer
	reconciler     Reconciler
}

// NewController creates a new controller framework instance.
func NewController(kubeClient kubernetes.Interface, reconciler Reconciler, controllerName string) *Controller {
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	informer := informerFactory.Resource().V1().ResourceClaims().Informer()

	c := &Controller{
		controllerName: controllerName,
		kubeClient:     kubeClient,
		informer:       informer,
		reconciler:     reconciler,
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addOrUpdate,
		UpdateFunc: func(old, new interface{}) { c.addOrUpdate(new) },
		DeleteFunc: c.delete,
	})

	return c
}

// Run starts the controller's reconciliation loop.
func (c *Controller) Run(ctx context.Context) {
	klog.Infof("Starting controller: %s", c.controllerName)
	go c.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		klog.Fatalf("Failed to sync cache for controller: %s", c.controllerName)
	}
	<-ctx.Done()
	klog.Infof("Shutting down controller: %s", c.controllerName)
}

// addOrUpdate handles the creation and update of ResourceSlices.
func (c *Controller) addOrUpdate(obj interface{}) {
	claim, ok := obj.(*resourcev1.ResourceClaim)
	if !ok {
		klog.Errorf("Invalid object type: %T", obj)
		return
	}

	if claim.Status.Allocation == nil {
		return
	}

	ctx := context.Background()
	deviceClass, err := c.kubeClient.ResourceV1().DeviceClasses().Get(ctx, claim.Spec.DeviceClassName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get DeviceClass %s for claim %s/%s: %v", claim.Spec.DeviceClassName, claim.Namespace, claim.Name, err)
		return
	}

	if !c.reconciler.IsDeviceClassRelevant(deviceClass) {
		return
	}

	klog.Infof("Reconciling ResourceClaim %s/%s", claim.Namespace, claim.Name)

	resourceSlice, err := c.reconciler.Reconcile(ctx, claim)
	if err != nil {
		klog.Errorf("Failed to reconcile ResourceClaim %s/%s: %v", claim.Namespace, claim.Name, err)
		return
	}

	resourceSlice.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(claim, resourcev1.SchemeGroupVersion.WithKind("ResourceClaim")),
	})

	_, err = c.kubeClient.ResourceV1().ResourceSlices().Create(ctx, resourceSlice, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			klog.Infof("ResourceSlice %s already exists", resourceSlice.Name)
		} else {
			klog.Errorf("Failed to create ResourceSlice %s: %v", resourceSlice.Name, err)
		}
	} else {
		klog.Infof("Successfully created ResourceSlice %s", resourceSlice.Name)
	}
}

// delete handles the deletion of ResourceSlices.
func (c *Controller) delete(obj interface{}) {
	claim, ok := obj.(*resourcev1.ResourceClaim)
	if !ok {
		// Handle case where the object is a tombstone
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Couldn't get object from tombstone %#v", obj)
			return
		}
		claim, ok = tombstone.Obj.(*resourcev1.ResourceClaim)
		if !ok {
			klog.Errorf("Tombstone contained object that is not a ResourceClaim %#v", obj)
			return
		}
	}

	klog.Infof("Deleting resources for ResourceClaim %s/%s", claim.Namespace, claim.Name)
	if err := c.reconciler.Delete(context.Background(), claim); err != nil {
		klog.Errorf("Failed to delete resources for claim %s/%s: %v", claim.Namespace, claim.Name, err)
	}
}
