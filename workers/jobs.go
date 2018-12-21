package workers

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/structs"

	m "github.com/sjeltuhin/clusterAgent/models"

	app "github.com/sjeltuhin/clusterAgent/appd"
	batchTypes "k8s.io/api/batch/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	batch "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type JobsWorker struct {
	informer       cache.SharedIndexInformer
	Client         *kubernetes.Clientset
	Bag            *m.AppDBag
	SummaryMap     map[string]m.ClusterJobMetrics
	WQ             workqueue.RateLimitingInterface
	AppdController *app.ControllerClient
	K8sConfig      *rest.Config
}

func NewJobsWorker(client *kubernetes.Clientset, bag *m.AppDBag, controller *app.ControllerClient, config *rest.Config) JobsWorker {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	pw := JobsWorker{Client: client, Bag: bag, SummaryMap: make(map[string]m.ClusterJobMetrics), WQ: queue, AppdController: controller, K8sConfig: config}
	pw.initJobInformer(client)
	return pw
}

func (nw *JobsWorker) initJobInformer(client *kubernetes.Clientset) cache.SharedIndexInformer {
	batchClient, err := batch.NewForConfig(nw.K8sConfig)
	if err != nil {
		fmt.Printf("Issues when initializing Batch API client/ %v", err)
		return nil
	}

	i := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return batchClient.Jobs(metav1.NamespaceAll).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return batchClient.Jobs(metav1.NamespaceAll).Watch(options)
			},
		},
		&v1.Node{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	i.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    nw.onNewJob,
		DeleteFunc: nw.onDeleteJob,
		UpdateFunc: nw.onUpdateJob,
	})
	nw.informer = i

	return i
}

func (nw *JobsWorker) onNewJob(obj interface{}) {
	jobObj := obj.(*v1.Node)
	fmt.Printf("Added Job: %s\n", jobObj.Name)

}

func (nw *JobsWorker) onDeleteJob(obj interface{}) {
	jobObj := obj.(*v1.Node)
	fmt.Printf("Deleted Job: %s\n", jobObj.Name)
}

func (nw *JobsWorker) onUpdateJob(objOld interface{}, objNew interface{}) {
	jobObj := objOld.(*v1.Node)
	fmt.Printf("Updated Job: %s\n", jobObj.Name)
}

func (pw JobsWorker) Observe(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	defer pw.WQ.ShutDown()
	wg.Add(1)
	go pw.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, pw.HasSynced) {
		fmt.Errorf("Timed out waiting for caches to sync")
	}
	fmt.Println("Cache syncronized. Starting the processing...")

	wg.Add(1)
	go pw.startMetricsWorker(stopCh)

	wg.Add(1)
	go pw.startEventQueueWorker(stopCh)

	<-stopCh
}

func (pw *JobsWorker) HasSynced() bool {
	return pw.informer.HasSynced()
}

func (pw *JobsWorker) startMetricsWorker(stopCh <-chan struct{}) {
	pw.appMetricTicker(stopCh, time.NewTicker(45*time.Second))

}

func (pw *JobsWorker) appMetricTicker(stop <-chan struct{}, ticker *time.Ticker) {
	for {
		select {
		case <-ticker.C:
			pw.buildAppDMetrics()
		case <-stop:
			ticker.Stop()
			return
		}
	}
}

func (pw *JobsWorker) buildAppDMetrics() {
	bth := pw.AppdController.StartBT("SendJobMetrics")
	pw.SummaryMap = make(map[string]m.ClusterJobMetrics)
	fmt.Println("Time to send job metrics. Current cache:")
	var count int = 0
	for _, obj := range pw.informer.GetStore().List() {
		jobObject := obj.(*batchTypes.Job)
		jobSchema := pw.processObject(jobObject)
		pw.summarize(&jobSchema)
		count++
	}
	fmt.Printf("Total: %d\n", count)

	ml := pw.builAppDMetricsList()

	fmt.Printf("Ready to push %d metrics\n", len(ml.Items))

	pw.AppdController.PostMetrics(ml)
	pw.AppdController.StopBT(bth)
}

func (pw *JobsWorker) processObject(j *batchTypes.Job) m.JobSchema {
	jobObject := m.NewJobObj()

	if j.ClusterName != "" {
		jobObject.ClusterName = j.ClusterName
	} else {
		jobObject.ClusterName = pw.Bag.AppName
	}
	jobObject.Name = j.Name
	jobObject.Namespace = j.Namespace

	var sb strings.Builder
	for k, v := range j.GetLabels() {
		fmt.Fprintf(&sb, "%s:%s;", k, v)
	}
	jobObject.Labels = sb.String()

	sb.Reset()
	for k, v := range j.GetAnnotations() {
		fmt.Fprintf(&sb, "%s:%s;", k, v)
	}
	jobObject.Annotations = sb.String()

	jobObject.Active = j.Status.Active

	jobObject.Success = j.Status.Succeeded

	jobObject.Failed = j.Status.Failed

	jobObject.StartTime = j.Status.StartTime.Time

	if j.Status.CompletionTime != nil {
		jobObject.EndTime = j.Status.CompletionTime.Time
		jobObject.Duration = jobObject.EndTime.Sub(jobObject.StartTime).Seconds()
	} else {
		jobObject.Duration = time.Since(jobObject.StartTime).Seconds()
	}

	jobObject.ActiveDeadlineSeconds = *j.Spec.ActiveDeadlineSeconds
	jobObject.Completions = *j.Spec.Completions
	jobObject.BackoffLimit = *j.Spec.BackoffLimit
	jobObject.Parallelism = *j.Spec.Parallelism

	return jobObject
}

func (pw *JobsWorker) summarize(jobObject *m.JobSchema) {
	//global metrics
	summary, ok := pw.SummaryMap[m.ALL]
	if !ok {
		summary = m.NewClusterJobMetrics(pw.Bag, m.ALL, m.ALL)
		pw.SummaryMap[m.ALL] = summary
	}

	//namespace metrics
	summaryNS, ok := pw.SummaryMap[jobObject.Namespace]
	if !ok {
		summaryNS = m.NewClusterJobMetrics(pw.Bag, m.ALL, jobObject.Namespace)
		pw.SummaryMap[jobObject.Namespace] = summaryNS
	}

	summary.JobCount++
	summaryNS.JobCount++

	summary.ActiveCount += int64(jobObject.Active)
	summary.FailedCount += int64(jobObject.Failed)
	summary.SuccessCount += int64(jobObject.Success)
	summary.Duration += int64(jobObject.Duration)

	summaryNS.ActiveCount += int64(jobObject.Active)
	summaryNS.FailedCount += int64(jobObject.Failed)
	summaryNS.SuccessCount += int64(jobObject.Success)
	summaryNS.Duration += int64(jobObject.Duration)
}

func (pw JobsWorker) builAppDMetricsList() m.AppDMetricList {
	ml := m.NewAppDMetricList()
	var list []m.AppDMetric
	for _, value := range pw.SummaryMap {
		pw.addMetricToList(&value, &list)
	}
	ml.Items = list
	return ml
}

func (pw JobsWorker) addMetricToList(metric *m.ClusterJobMetrics, list *[]m.AppDMetric) {
	objMap := structs.Map(metric)
	for fieldName, fieldValue := range objMap {
		if fieldName != "Namespace" && fieldName != "Path" && fieldName != "Metadata" {
			appdMetric := m.NewAppDMetric(fieldName, fieldValue.(int64), metric.Path)
			*list = append(*list, appdMetric)
		}
	}
}

//queue

func (pw *JobsWorker) startEventQueueWorker(stopCh <-chan struct{}) {
	pw.eventQueueTicker(stopCh, time.NewTicker(15*time.Second))
}

func (pw *JobsWorker) eventQueueTicker(stop <-chan struct{}, ticker *time.Ticker) {
	for {
		select {
		case <-ticker.C:
			pw.flushQueue()
		case <-stop:
			ticker.Stop()
			return
		}
	}
}

func (pw *JobsWorker) flushQueue() {
	bth := pw.AppdController.StartBT("FlushJobEventsQueue")
	count := pw.WQ.Len()
	fmt.Printf("Flushing the queue of %d records", count)
	if count == 0 {
		return
	}

	var objList []m.JobSchema
	var jobRecord *m.JobSchema
	var ok bool = true

	for count >= 0 {

		jobRecord, ok = pw.getNextQueueItem()
		count = count - 1
		if ok {
			objList = append(objList, *jobRecord)
		} else {
			fmt.Println("Queue shut down")
		}
		if count == 0 || len(objList) >= pw.Bag.EventAPILimit {
			fmt.Printf("Sending %d records to AppD events API\n", len(objList))
			pw.postJobRecords(&objList)
			return
		}
	}
	pw.AppdController.StopBT(bth)
}

func (pw *JobsWorker) postJobRecords(objList *[]m.JobSchema) {
	logger := log.New(os.Stdout, "[APPD_CLUSTER_MONITOR]", log.Lshortfile)
	rc := app.NewRestClient(pw.Bag, logger)
	data, err := json.Marshal(objList)
	schemaDefObj := m.NewPodSchemaDefWrapper()
	schemaDef, e := json.Marshal(schemaDefObj)
	fmt.Printf("Schema def: %s\n", string(schemaDef))
	if err == nil && e == nil {
		if rc.SchemaExists(pw.Bag.JobSchemaName) == false {
			fmt.Printf("Creating schema. %s\n", pw.Bag.JobSchemaName)
			if rc.CreateSchema(pw.Bag.JobSchemaName, schemaDef) != nil {
				fmt.Printf("Schema %s created\n", pw.Bag.JobSchemaName)
			}
		} else {
			fmt.Printf("Schema %s exists\n", pw.Bag.JobSchemaName)
		}
		fmt.Println("About to post records")
		rc.PostAppDEvents(pw.Bag.JobSchemaName, data)
	} else {
		fmt.Printf("Problems when serializing array of pod schemas. %v", err)
	}
}

func (pw *JobsWorker) getNextQueueItem() (*m.JobSchema, bool) {
	podRecord, quit := pw.WQ.Get()

	if quit {
		return podRecord.(*m.JobSchema), false
	}
	defer pw.WQ.Done(podRecord)
	pw.WQ.Forget(podRecord)

	return podRecord.(*m.JobSchema), true
}
