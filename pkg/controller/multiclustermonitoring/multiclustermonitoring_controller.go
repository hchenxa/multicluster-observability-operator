package multiclustermonitoring

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	monitoringv1 "github.com/open-cluster-management/multicluster-monitoring-operator/pkg/apis/monitoring/v1"
	"github.com/open-cluster-management/multicluster-monitoring-operator/pkg/rendering"
)

var log = logf.Log.WithName("controller_multiclustermonitoring")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new MultiClusterMonitoring Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileMultiClusterMonitoring{client: mgr.GetClient(), apiReader: mgr.GetAPIReader(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("multiclustermonitoring-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource MultiClusterMonitoring
	err = c.Watch(&source.Kind{Type: &monitoringv1.MultiClusterMonitoring{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner MultiClusterMonitoring
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &monitoringv1.MultiClusterMonitoring{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileMultiClusterMonitoring implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileMultiClusterMonitoring{}

// ReconcileMultiClusterMonitoring reconciles a MultiClusterMonitoring object
type ReconcileMultiClusterMonitoring struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	apiReader client.Reader
	scheme    *runtime.Scheme
}

// Reconcile reads that state of the cluster for a MultiClusterMonitoring object and makes changes based on the state read
// and what is in the MultiClusterMonitoring.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileMultiClusterMonitoring) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling MultiClusterMonitoring")

	// Fetch the MultiClusterMonitoring instance
	instance := &monitoringv1.MultiClusterMonitoring{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	//Render the templates with a specified CR
	renderer := rendering.NewRenderer(instance)
	toDeploy, err := renderer.Render(r.client)
	if err != nil {
		reqLogger.Error(err, "Failed to render multiClusterMonitoring templates")
		return reconcile.Result{}, err
	}
	//Deploy the resources
	for _, res := range toDeploy {
		if res.GetNamespace() == instance.Namespace {
			if err := controllerutil.SetControllerReference(instance, res, r.scheme); err != nil {
				reqLogger.Error(err, "Failed to set controller reference")
			}
		}
		if err := deploy(r.client, res); err != nil {
			reqLogger.Error(err, fmt.Sprintf("Failed to deploy %s %s/%s", res.GetKind(), instance.Namespace, res.GetName()))
			return reconcile.Result{}, err
		}
	}

	// create a Observatorium CR
	result, err := GenerateObservatoriumCR(r.client, r.scheme, instance)
	if result != nil {
		return *result, err
	}

	// expose observatorium api gateway
	result, err = GenerateAPIGatewayRoute(r.client, r.scheme, instance)
	if result != nil {
		return *result, err
	}
	// generate grafana datasource to point to observatorium api gateway
	result, err = GenerateGrafanaDataSource(r.client, r.scheme, instance)
	if result != nil {
		return *result, err
	}

	// generate/update the configmap cluster-monitoring-config
	result, err = UpdateOCPMonitoringCM(instance)
	if result != nil {
		return *result, err
	}

	result, err = r.UpdateStatus(instance)
	if result != nil {
		return *result, err
	}

	return reconcile.Result{Requeue: true}, nil
}

func (r *ReconcileMultiClusterMonitoring) UpdateStatus(mcm *monitoringv1.MultiClusterMonitoring) (*reconcile.Result, error) {
	err := r.client.Status().Update(context.TODO(), mcm)
	if err != nil {
		if errors.IsConflict(err) {
			// Error from object being modified is normal behavior and should not be treated like an error
			log.Info("Failed to update status", "Reason", "Object has been modified")
			return &reconcile.Result{RequeueAfter: time.Second}, nil
		}

		log.Error(err, fmt.Sprintf("Failed to update %s/%s status ", mcm.Namespace, mcm.Name))
		return &reconcile.Result{}, err
	}
	return &reconcile.Result{}, nil
}

func deploy(c client.Client, obj *unstructured.Unstructured) error {
	found := &unstructured.Unstructured{}
	found.SetGroupVersionKind(obj.GroupVersionKind())
	err := c.Get(context.TODO(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, found)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.Create(context.TODO(), obj)
		}
		return err
	}

	if found.GetKind() != "Deployment" {
		return nil
	}

	oldSpec, oldSpecFound := found.Object["spec"]
	newSpec, newSpecFound := obj.Object["spec"]
	if !oldSpecFound || !newSpecFound {
		return nil
	}
	if !reflect.DeepEqual(oldSpec, newSpec) {
		newObj := found.DeepCopy()
		newObj.Object["spec"] = newSpec
		return c.Update(context.TODO(), newObj)
	}
	return nil
}
