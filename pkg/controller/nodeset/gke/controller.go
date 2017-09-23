/*
Copyright 2017 The Kubernetes Authors.

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

package gke

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	gce "google.golang.org/api/compute/v1"
	gke "google.golang.org/api/container/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	nodesetclientset "github.com/kube-node/nodeset/pkg/client/clientset/versioned"
	nodesetinformers "github.com/kube-node/nodeset/pkg/client/informers/externalversions/nodeset/v1alpha1"
	nodesetlisters "github.com/kube-node/nodeset/pkg/client/listers/nodeset/v1alpha1"
	"github.com/kube-node/nodeset/pkg/nodeset/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	nodeAutoprovisioningPrefix = "nodeautoprovisioning"
)

type Controller struct {
	nodesetClientset nodesetclientset.Interface
	nodesetLister    nodesetlisters.NodeSetLister
	nodesetIndexer   cache.Indexer
	nodesetsSynched  cache.InformerSynced

	name    string
	gke     *gke.Service
	gce     *gce.Service
	cluster Cluster

	// queue is where incoming work is placed to de-dup and to allow "easy"
	// rate limited requeues on errors
	queue workqueue.RateLimitingInterface
}

func New(name string, clusterName string, client nodesetclientset.Interface, nodesets nodesetinformers.NodeSetInformer) (*Controller, error) {
	// index nodesets by uids
	// TODO: move outside of New
	nodesets.Informer().AddIndexers(map[string]cache.IndexFunc{
		UIDIndex: MetaUIDIndexFunc,
	})

	c := &Controller{
		nodesetLister:   nodesets.Lister(),
		nodesetIndexer:  nodesets.Informer().GetIndexer(),
		nodesetsSynched: nodesets.Informer().HasSynced,

		name: name,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "nodeset"),
	}

	// register event handlers to fill the queue with nodeset creations, updates and deletions
	nodesets.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				c.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta nodesetQueue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
	})

	// get GKE client
	var err error
	c.gke, c.gce, c.cluster, err = NewGKEService(clusterName)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer c.queue.ShutDown()

	glog.Infof("Starting <NAME> controller")

	// wait for your secondary caches to fill before starting your work
	if !cache.WaitForCacheSync(stopCh, c.nodesetsSynched) {
		return
	}

	// start up your worker threads based on threadiness.  Some controllers
	// have multiple kinds of workers
	for i := 0; i < threadiness; i++ {
		// runWorker will loop until "something bad" happens.  The .Until will
		// then rekick the worker after one second
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	go wait.Until(c.runReconciler, time.Minute, stopCh)

	// wait until we're told to stop
	<-stopCh
	glog.Infof("Shutting down <NAME> controller")
}

func (c *Controller) runWorker() {
	// hot loop until we're told to stop.  processNextWorkItem will
	// automatically wait until there's work available, so we don't worry
	// about secondary waits
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false
// when it's time to quit.
func (c *Controller) processNextWorkItem() bool {
	// pull the next work item from queue.  It should be a key we use to lookup
	// something in a cache
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// you always have to indicate to the queue that you've completed a piece of
	// work
	defer c.queue.Done(key)

	// do your work on the key.  This method will contains your "do stuff" logic
	err := c.syncHandler(key.(string))
	if err == nil {
		// if you had no error, tell the queue to stop tracking history for your
		// key. This will reset things like failure counts for per-item rate
		// limiting
		c.queue.Forget(key)
		return true
	}

	// there was a failure so be sure to report it.  This method allows for
	// pluggable error handling which can be used for things like
	// Cluster-monitoring
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	// since we failed, we should requeue the item to work on later.  This
	// method will add a backoff to avoid hotlooping on particular items
	// (they're probably still not going to work right away) and overall
	// controller protection (everything I've done is broken, this controller
	// needs to calm down or it can starve other useful work) cases.
	c.queue.AddRateLimited(key)

	return true
}

func (c *Controller) syncHandler(key string) error {
	nodeset, err := c.nodesetLister.Get(key)
	if apierrors.IsNotFound(err) {
		glog.V(0).Infof("Pod %s was not found: %v", key, err)
		return nil
	}
	if err != nil {
		return err
	}

	if nodeset.Spec.NodeSetController != c.name {
		return nil
	}

	glog.Infof("NodeSet seen %q", nodeset.Name)

	return nil
}

func (c *Controller) runReconciler() {
	for c.reconsileNodeSets() {
	}
}

func (c *Controller) reconsileNodeSets() bool {
	resp, err := c.gke.Projects.Zones.Clusters.NodePools.List(c.cluster.Project, c.cluster.Zone, c.cluster.Name).Do()
	if err != nil {
		glog.Warningf("NodePool reconcile error: %v", err)
		return true
	}

	for _, pool := range resp.NodePools {
		/*autoprovisioned := strings.Contains(pool.Name, nodeAutoprovisioningPrefix)
		autoscaled := pool.Autoscaling != nil && nodePool.Autoscaling.Enabled
		if !autoprovisioned && !autoscaled {
			continue
		}
		// format is
		// "https://www.googleapis.com/compute/v1/projects/mwielgus-proj/zones/europe-west1-b/instanceGroupManagers/gke-cluster-1-default-pool-ba78a787-grp"
		for _, igurl := range nodePool.InstanceGroupUrls {
			project, zone, name, err := parseGceUrl(igurl, "instanceGroupManagers")
			if err != nil {
				return err
			}
			mig := &Mig{
				GceRef: GceRef{
					Name:    name,
					Zone:    zone,
					Project: project,
				},
				gceManager:      m,
				exist:           true,
				autoprovisioned: autoprovisioned,
				nodePoolName:    nodePool.Name,
			}
			existingMigs[mig.GceRef] = struct{}{}

			if autoscaled {
				mig.minSize = int(nodePool.Autoscaling.MinNodeCount)
				mig.maxSize = int(nodePool.Autoscaling.MaxNodeCount)
			} else if autoprovisioned {
				mig.minSize = minAutoprovisionedSize
				mig.maxSize = maxAutoprovisionedSize
			}
			m.RegisterMig(mig)
		}
		*/

		_, err := c.nodesetLister.Get(pool.Name)
		if apierrors.IsNotFound(err) {
			_, err = c.nodesetClientset.NodesetV1alpha1().NodeSets().Create(&v1alpha1.NodeSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: pool.Name,
				},
				Spec: v1alpha1.NodeSetSpec{
					NodeSetController: c.name,
					MaxSurge:          &intstr.FromInt(1),
					//Replicas: pool.
				},
			})
			continue
		}
	}

	return true
}

const (
	UIDIndex string = "uid"
)

// MetaUIDIndexFunc indexes by uid.
func MetaUIDIndexFunc(obj interface{}) ([]string, error) {
	meta, err := meta.Accessor(obj)
	if err != nil {
		return []string{""}, fmt.Errorf("object has no meta: %v", err)
	}
	return []string{string(meta.GetUID())}, nil
}
