/*
Copyright 2017 The Kubernetes Authors.
Copyright 2023 Autovia GmbH.

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

package main

import (
	"context"
	"fmt"
	"time"

	kanikov1alpha1 "autovia.io/kaniko-controller/pkg/apis/kanikocontroller/v1alpha1"
	clientset "autovia.io/kaniko-controller/pkg/generated/clientset/versioned"
	kanikoscheme "autovia.io/kaniko-controller/pkg/generated/clientset/versioned/scheme"
	informers "autovia.io/kaniko-controller/pkg/generated/informers/externalversions/kanikocontroller/v1alpha1"
	listers "autovia.io/kaniko-controller/pkg/generated/listers/kanikocontroller/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const controllerAgentName = "kaniko-controller"

const (
	SuccessSynced         = "Synced"
	ErrResourceExists     = "ErrResourceExists"
	MessageResourceExists = "Resource %q already exists and is not managed by Image"
	MessageResourceSynced = "Image synced successfully"
)

type Controller struct {
	kubeclientset   kubernetes.Interface
	kanikoclientset clientset.Interface
	podsLister      corelisters.PodLister
	podsSynced      cache.InformerSynced
	imagesLister    listers.ImageLister
	imagesSynced    cache.InformerSynced
	workqueue       workqueue.RateLimitingInterface
	recorder        record.EventRecorder
}

func NewController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	kanikoclientset clientset.Interface,
	podInformer coreinformers.PodInformer,
	imageInformer informers.ImageInformer) *Controller {
	logger := klog.FromContext(ctx)
	utilruntime.Must(kanikoscheme.AddToScheme(scheme.Scheme))
	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:   kubeclientset,
		kanikoclientset: kanikoclientset,
		podsLister:      podInformer.Lister(),
		podsSynced:      podInformer.Informer().HasSynced,
		imagesLister:    imageInformer.Lister(),
		imagesSynced:    imageInformer.Informer().HasSynced,
		workqueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Images"),
		recorder:        recorder,
	}

	logger.Info("Setting up event handlers")
	imageInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueImage,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueImage(new)
		},
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*corev1.Pod)
			oldDepl := old.(*corev1.Pod)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})
	return controller
}

func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)
	logger.Info("Starting Image controller")
	logger.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.podsSynced, c.imagesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count", workers)
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logger.Info("Started workers")
	<-ctx.Done()
	logger.Info("Shutting down workers")

	return nil
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.syncHandler(ctx, key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		logger.Info("Successfully synced", "resourceName", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	image, err := c.imagesLister.Images(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("Image '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	dockerfile := image.Spec.Dockerfile
	if dockerfile == "" {
		utilruntime.HandleError(fmt.Errorf("%s: dockerfile name must be specified", key))
		return nil
	}

	pod, err := c.podsLister.Pods(image.Namespace).Get(fmt.Sprintf("kaniko-%s", image.Name))
	if errors.IsNotFound(err) {
		pod, err = c.kubeclientset.CoreV1().Pods(image.Namespace).Create(context.TODO(), newPod(image), metav1.CreateOptions{})
	}

	if err != nil {
		return err
	}

	if !metav1.IsControlledBy(pod, image) {
		msg := fmt.Sprintf(MessageResourceExists, pod.Name)
		c.recorder.Event(image, corev1.EventTypeWarning, ErrResourceExists, msg)
		return fmt.Errorf("%s", msg)
	}

	c.recorder.Event(image, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Controller) updateImageStatus(image *kanikov1alpha1.Image, pod *corev1.Pod) error {
	imageCopy := image.DeepCopy()
	imageCopy.Spec.Status = string(pod.Status.Phase)
	_, err := c.kanikoclientset.KanikocontrollerV1alpha1().Images(image.Namespace).Update(context.TODO(), imageCopy, metav1.UpdateOptions{})
	return err
}

func (c *Controller) enqueueImage(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	logger := klog.FromContext(context.Background())
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		logger.V(4).Info("Recovered deleted object", "resourceName", object.GetName())
	}
	logger.V(4).Info("Processing object", "object", klog.KObj(object))
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		if ownerRef.Kind != "Image" {
			return
		}

		image, err := c.imagesLister.Images(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			logger.V(4).Info("Ignore orphaned object", "object", klog.KObj(object), "image", ownerRef.Name)
			return
		}

		logger.Info(fmt.Sprintf("image name %+v", image.Name))
		if pod, ok := obj.(*corev1.Pod); ok {
			logger.Info(fmt.Sprintf("pod name %+v", pod.Name))
			logger.Info(fmt.Sprintf("pod status %+v", pod.Status.Phase))
			if string(pod.Status.Phase) != image.Spec.Status {
				err = c.updateImageStatus(image, pod)
				if err != nil {
					logger.V(4).Info("Can not update image", image.Name)
					return
				}
			}
		}
		c.enqueueImage(image)

		return
	}
}

func newPod(image *kanikov1alpha1.Image) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("kaniko-%s", image.Name),
			Namespace: image.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(image, kanikov1alpha1.SchemeGroupVersion.WithKind("Image")),
			},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{
					Name:  "kaniko-init",
					Image: "busybox:1.28",
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf("cat <<EOF > /workspace/dockerfile\n%s\nEOF", image.Spec.Dockerfile),
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "dockerfile-storage",
							MountPath: "/workspace",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "kaniko",
					Image: "gcr.io/kaniko-project/executor:latest",
					Args: []string{
						"--dockerfile=/workspace/dockerfile",
						"--context=dir://workspace",
						"--destination=" + image.Spec.Destination,
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "kaniko-secret",
							MountPath: "/kaniko/.docker",
						},
						{
							Name:      "dockerfile-storage",
							MountPath: "/workspace",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "kaniko-secret",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "regcred",
							Items: []corev1.KeyToPath{
								{
									Key:  ".dockerconfigjson",
									Path: "config.json",
								},
							},
						},
					},
				},
				{
					Name: "dockerfile-storage",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "dockerfile-claim",
						},
					},
				},
			},
		},
	}
}
