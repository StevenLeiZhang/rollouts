/*
Copyright 2019 The Kruise Authors.
Copyright 2016 The Kubernetes Authors.

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

package deployment

import (
	"context"
	"encoding/json"
	"flag"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	rolloutsv1alpha1 "github.com/openkruise/rollouts/api/v1alpha1"
	deploymentutil "github.com/openkruise/rollouts/pkg/controller/deployment/util"
	"github.com/openkruise/rollouts/pkg/feature"
	clientutil "github.com/openkruise/rollouts/pkg/util/client"
	utilfeature "github.com/openkruise/rollouts/pkg/util/feature"
)

func init() {
	flag.IntVar(&concurrentReconciles, "deployment-workers", concurrentReconciles, "Max concurrent workers for StatefulSet controller.")
}

var (
	concurrentReconciles = 3
)

// Add creates a new StatefulSet Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(feature.AdvancedDeploymentGate) {
		klog.Warningf("Advanced deployment controller is disabled")
		return nil
	}
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	cacher := mgr.GetCache()
	podInformer, err := cacher.GetInformerForKind(context.TODO(), v1.SchemeGroupVersion.WithKind("Pod"))
	if err != nil {
		return nil, err
	}
	dInformer, err := cacher.GetInformerForKind(context.TODO(), appsv1.SchemeGroupVersion.WithKind("Deployment"))
	if err != nil {
		return nil, err
	}
	rsInformer, err := cacher.GetInformerForKind(context.TODO(), appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
	if err != nil {
		return nil, err
	}

	// Lister
	dLister := appslisters.NewDeploymentLister(dInformer.(toolscache.SharedIndexInformer).GetIndexer())
	rsLister := appslisters.NewReplicaSetLister(rsInformer.(toolscache.SharedIndexInformer).GetIndexer())
	podLister := corelisters.NewPodLister(podInformer.(toolscache.SharedIndexInformer).GetIndexer())

	// Client & Recorder
	genericClient := clientutil.GetGenericClientWithName("advanced-deployment-controller")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: genericClient.KubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "advanced-deployment-controller"})

	// Deployment controller factory
	factory := &controllerFactory{
		client:           genericClient.KubeClient,
		eventBroadcaster: eventBroadcaster,
		eventRecorder:    recorder,
		dLister:          dLister,
		rsLister:         rsLister,
		podLister:        podLister,
	}
	return &ReconcileDeployment{Client: mgr.GetClient(), controllerFactory: factory}, nil
}

var _ reconcile.Reconciler = &ReconcileDeployment{}

// ReconcileDeployment reconciles a Deployment object
type ReconcileDeployment struct {
	// client interface
	client.Client
	controllerFactory *controllerFactory
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("advanced-deployment-controller", mgr, controller.Options{
		Reconciler: r, MaxConcurrentReconciles: concurrentReconciles})
	if err != nil {
		return err
	}

	if err = c.Watch(&source.Kind{Type: &appsv1.ReplicaSet{}}, &handler.EnqueueRequestForOwner{
		IsController: true, OwnerType: &appsv1.ReplicaSet{}}, predicate.Funcs{}); err != nil {
		return err
	}

	// TODO: handle deployment only when the deployment is under our control
	updateHandler := func(e event.UpdateEvent) bool {
		oldObject := e.ObjectOld.(*appsv1.Deployment)
		newObject := e.ObjectNew.(*appsv1.Deployment)
		if !deploymentutil.IsUnderRolloutControl(newObject) {
			return false
		}
		if oldObject.Generation != newObject.Generation || newObject.DeletionTimestamp != nil {
			klog.V(3).Infof("Observed updated Spec for Deployment: %s/%s", newObject.Namespace, newObject.Name)
			return true
		}
		if len(oldObject.Annotations) != len(newObject.Annotations) || !reflect.DeepEqual(oldObject.Annotations, newObject.Annotations) {
			klog.V(3).Infof("Observed updated Annotation for Deployment: %s/%s", newObject.Namespace, newObject.Name)
			return true
		}
		return false
	}

	// Watch for changes to Deployment
	return c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForObject{}, predicate.Funcs{UpdateFunc: updateHandler})
}

// Reconcile reads that state of the cluster for a Deployment object and makes changes based on the state read
// and what is in the Deployment.Spec and Deployment.Annotations
// Automatically generate RBAC rules to allow the Controller to read and write ReplicaSets
func (r *ReconcileDeployment) Reconcile(_ context.Context, request reconcile.Request) (reconcile.Result, error) {
	deployment := new(appsv1.Deployment)
	err := r.Get(context.TODO(), request.NamespacedName, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// TODO: create new controller only when deployment is under our control
	dc := r.controllerFactory.NewController(deployment)
	if dc == nil {
		return reconcile.Result{}, nil
	}

	err = dc.syncDeployment(context.Background(), deployment)
	return ctrl.Result{}, err
}

type controllerFactory DeploymentController

// NewController create a new DeploymentController
// TODO: create new controller only when deployment is under our control
func (f *controllerFactory) NewController(deployment *appsv1.Deployment) *DeploymentController {
	if !deploymentutil.IsUnderRolloutControl(deployment) {
		klog.Warningf("Deployment %v is not under rollout control, ignore", klog.KObj(deployment))
		return nil
	}

	strategy := rolloutsv1alpha1.DeploymentStrategy{}
	strategyAnno := deployment.Annotations[rolloutsv1alpha1.DeploymentStrategyAnnotation]
	if err := json.Unmarshal([]byte(strategyAnno), &strategy); err != nil {
		klog.Errorf("Failed to unmarshal strategy for deployment %v: %v", klog.KObj(deployment), strategyAnno)
		return nil
	}

	// We do NOT process such deployment with canary rolling style
	if strategy.RollingStyle == rolloutsv1alpha1.CanaryRollingStyleType {
		return nil
	}

	marshaled, _ := json.Marshal(&strategy)
	klog.V(4).Infof("Processing deployment %v strategy %v", klog.KObj(deployment), string(marshaled))

	return &DeploymentController{
		client:           f.client,
		eventBroadcaster: f.eventBroadcaster,
		eventRecorder:    f.eventRecorder,
		dLister:          f.dLister,
		rsLister:         f.rsLister,
		podLister:        f.podLister,
		strategy:         strategy,
	}
}
