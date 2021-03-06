package main

import (
	"fmt"
	"time"

	"database/sql"

	"github.com/golang/glog"
	_ "github.com/lib/pq"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	dbv1alpha1 "github.com/joshrendek/k8s-external-postgres/pkg/apis/postgresql/v1"
	v1 "github.com/joshrendek/k8s-external-postgres/pkg/apis/postgresql/v1"
	clientset "github.com/joshrendek/k8s-external-postgres/pkg/client/clientset/versioned"
	samplescheme "github.com/joshrendek/k8s-external-postgres/pkg/client/clientset/versioned/scheme"
	informers "github.com/joshrendek/k8s-external-postgres/pkg/client/informers/externalversions"
	listers "github.com/joshrendek/k8s-external-postgres/pkg/client/listers/postgresql/v1"
	"github.com/rs/zerolog/log"
)

const controllerAgentName = "sample-controller-foobar"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Foo is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Foo fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Foo"
	// MessageResourceSynced is the message used for an Event fired when a Foo
	// is synced successfully
	MessageResourceSynced = "Foo synced successfully"
)

// Controller is the controller implementation for Foo resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// databaseClientset is a clientset for our own API group
	databaseClientset clientset.Interface

	DatabasesLister listers.DatabaseLister
	DatabasesSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
	DB       *sql.DB
}

// NewController returns a new sample controller
func NewController(
	kubeclientset kubernetes.Interface,
	databaseClientset clientset.Interface,
	databaseInformerFactory informers.SharedInformerFactory) *Controller {

	// obtain references to shared index informers for the Deployment and Foo
	// types.
	databaseInformer := databaseInformerFactory.Databases().V1().Databases()

	// Create event broadcaster
	// Add sample-controller types to the default Kubernetes Scheme so Events can be
	// logged for sample-controller types.
	samplescheme.AddToScheme(scheme.Scheme)
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	db, err := sql.Open("postgres", postgresURL)
	if err != nil {
		panic(err)
	}

	if err := db.Ping(); err != nil {
		panic(err)
	}

	controller := &Controller{
		kubeclientset:     kubeclientset,
		databaseClientset: databaseClientset,
		DatabasesLister:   databaseInformer.Lister(),
		DatabasesSynced:   databaseInformer.Informer().HasSynced,
		workqueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Foos"),
		recorder:          recorder,
		DB:                db,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when Foo resources change
	databaseInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueDatabase,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueDatabase(new)
		},
		// can't call enqueueDatabase since it'll be deleted by the time the work queue gets it,
		// handle it immediately instead
		DeleteFunc: func(obj interface{}) {
			dbResource := obj.(*v1.Database)

			dbStmt := fmt.Sprintf("DROP DATABASE %s", dbResource.Spec.Database)
			if _, err := db.Exec(dbStmt); err != nil {
				fmt.Println("error deleting database: ", err)
			}

			stmt := fmt.Sprintf("DROP ROLE %s", dbResource.Spec.Username)
			if _, err := db.Exec(stmt); err != nil {
				fmt.Println("error dropping user: ", err)
			}
			log.Debug().Str("database", dbResource.Spec.Database).Msg("dropping database")
		},
	})
	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting Database controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.DatabasesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process Foo resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Foo resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the database resource with this namespace/name
	dbResource, err := c.DatabasesLister.Databases(namespace).Get(name)
	if err != nil {
		// The Foo resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("dbResource '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	username := dbResource.Spec.Username
	password := dbResource.Spec.Password
	database := dbResource.Spec.Database

	switch dbResource.Status.State {
	case "provisioned":
		log.Debug().Str("username", username).Str("database", database).Msg("already provisioned")
	case "error":
		log.Debug().Str("error", dbResource.Status.Message).Msg("error provisioning")
	default:
		log.Debug().Str("username", username).
			Str("password", password).
			Str("database", database).
			Msg("provisioning")

		stmt := fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s'", username, password)
		if _, err := c.DB.Exec(stmt); err != nil {
			if err := c.updateFooStatus(dbResource, fmt.Sprintf("Error creating user: %s", err.Error()), "error"); err != nil {
				return err
			}
			fmt.Println("error creating user: ", err)
		}

		dbStmt := fmt.Sprintf("CREATE DATABASE %s OWNER %s", database, username)
		if _, err := c.DB.Exec(dbStmt); err != nil {
			if err := c.updateFooStatus(dbResource, fmt.Sprintf("Error creating database: %s", err.Error()), "error"); err != nil {
				return err
			}
		}

		if err := c.updateFooStatus(dbResource, "successful", "provisioned"); err != nil {
			return err
		}
	}
	c.recorder.Event(dbResource, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Controller) updateFooStatus(dbResource *dbv1alpha1.Database, message, state string) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	dbCopy := dbResource.DeepCopy()
	dbCopy.Status.Message = message
	dbCopy.Status.State = state
	// If the CustomResourceSubresources feature gate is not enabled,
	// we must use Update instead of UpdateStatus to update the Status block of the Foo resource.
	// UpdateStatus will not allow changes to the Spec of the resource,
	// which is ideal for ensuring nothing other than resource status has been updated.
	_, err := c.databaseClientset.DatabasesV1().Databases(dbResource.Namespace).Update(dbCopy)
	return err
}

// enqueueDatabase takes a Foo resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Foo.
func (c *Controller) enqueueDatabase(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}
