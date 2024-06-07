/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	kbatch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/reference"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robfig/cron"

	batchv1 "kubebuilderProject/api/v1"
)

var (
	scheduledTimeAnnotation = "batch.zyc.io/scheduled-at"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

type realClock struct{}

func (_ realClock) Now() time.Time {
	return time.Now()
}

type Clock interface {
	Now() time.Time
}

// +kubebuilder:rbac:groups=batch.zyc.io,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch.zyc.io,resources=cronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch.zyc.io,resources=cronjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.2/pkg/reconcile
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. 通过name获取cronJob对象
	var cronJob batchv1.CronJob
	// 注：这里的Get方法client.Client类的方法，它的使用方法和client-go中的clisntset的使用方法不一样
	if err := r.Get(ctx, req.NamespacedName, &cronJob); err != nil {
		logger.Error(err, "unable to fetch CronJob")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. List所有active的jobs，然后update status
	var childJobs kbatch.JobList

	// jobOwnerKey实际上是一个index，可以更快地list符合条件的Object
	// 由于cronJob管理K8s原生的Job，因此这里也需要把原生Job和cronJob创建的Job做区分
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		logger.Error(err, "unable to list child Jobs")
		// 注：虽然Result中没有注明Requeue: true，但是返回了err，也会触发Requeue操作
		return ctrl.Result{}, err
	}

	// isJobFinished 判断一个任务是否完成
	isJobFinished := func(job *kbatch.Job) (bool, kbatch.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}

		return false, ""
	}

	getScheduledTimeForJob := func(job *kbatch.Job) (*time.Time, error) {
		timeRaw := job.Annotations[scheduledTimeAnnotation]
		if len(timeRaw) == 0 {
			return nil, nil
		}
		timeParsed, err := time.Parse(time.RFC3339, timeRaw)
		if err != nil {
			return nil, err
		}
		return &timeParsed, nil
	}

	var activeJobs, failedJobs, successfulJobs []*kbatch.Job
	var mostRecentTime *time.Time // find the last run so we can update the status

	// 遍历所有的childJob，
	// 1. 把他们分成三类：activeJob、failedJob和successfulJob
	// 2. 对于每个job，获取scheduledTime,然后更新最晚的调度时间，把它写入mostRecentTime中
	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "":
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case kbatch.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case kbatch.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}

		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			logger.Error(err, "unable to parse time for child job", "job", &job)
			continue
		}

		// 一般来说这里scheduledTimeForJob不会为空
		if scheduledTimeForJob != nil {
			// 将mostRecentTime更新为最新的执行ScheduledTime
			if mostRecentTime == nil || mostRecentTime.Before(*scheduledTimeForJob) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	// 更新cronJob中的时间
	if mostRecentTime != nil {
		cronJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		cronJob.Status.LastScheduleTime = nil
	}

	// cronJob status中的活跃Job信息回填, 填充完毕后调用client的update接口回写即可
	cronJob.Status.Active = nil
	for _, activeJob := range activeJobs {
		jobRef, err := reference.GetReference(r.Scheme, activeJob)
		if err != nil {
			logger.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	}

	logger.V(1).Info("job count", "active jobs", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))

	// 此处的update会触发Reconcile
	if err := r.Status().Update(ctx, &cronJob); err != nil {
		logger.Error(err, "unable to update cronJob status")
		return ctrl.Result{}, err
	}

	// 3. 根据设置的history limit清理old jobs
	// 3.1 FailedJob
	if cronJob.Spec.FailedJobsHistoryLimit != nil {
		// 保证nil是最小的，且按照时间从小大大排序
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*cronJob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "unable to delete old failed job", job)
			} else {
				logger.V(0).Info("deleted old failed job", job)
			}
		}
	}

	// 3.2 SuccessJob
	if cronJob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i, job := range successfulJobs {
			if int32(i) >= int32(len(successfulJobs))-*cronJob.Spec.SuccessfulJobsHistoryLimit {
				if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
					logger.Error(err, "unable to delete old successful job")
				} else {
					logger.V(0).Info("deleted old success job", "job", job)
				}
			}
		}
	}

	// 4. 如果cronJOb的状态为suspended，直接跳过。这有助于我们去排查cronJob为suspend的原因
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		logger.V(1).Info("cronJob suspend, skipping")
		return ctrl.Result{}, nil
	}

	// 5. Get the next scheduled run
	// 我们需要计算下一次运行Job的时间。注意，cron表达式的解析是有库函数可供调用的.
	// nextRun用来计算下一次调用Reconcile的时间，missedRun才是本次Reconcile的时间
	missedRun, nextRun, err := getNextSchedule(&cronJob, r.Now())
	if err != nil {
		logger.Error(err, "unable to figure out CronJob schedule")
		// we don't really care about requeuing until we get an update that
		// fixes the schedule, so don't return an error
		return ctrl.Result{}, nil
	}

	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())}
	logger = logger.WithValues("now", r.Now(), "next run", nextRun)

	// 6. 运行一个Job
	// 6.1 检查missedRun，若为0，那么直接返回之前的scheduledResult
	if missedRun.IsZero() {
		logger.V(1).Info("no upcoming scheduled times, sleeping until next")
		return scheduledResult, nil
	}
	// 6.2 若有missedRun，且仍然在deadline之前，那么要补上这个任务;否则（tooLate = true）就跳过这次任务了
	tooLate := false
	if cronJob.Spec.StartingDeadlineSeconds != nil {
		tooLate = missedRun.Add(time.Duration(*cronJob.Spec.StartingDeadlineSeconds) * time.Second).Before(r.Now())
	}
	if tooLate {
		logger.V(1).Info("missed starting deadline for last run, sleeping till next")
		return scheduledResult, nil
	}
	// 6.3 根据Concurrency策略执行不同操作：ForbidConcurrent不允许多个activeJob；ReplaceConcurrent会删除所有activeJob
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(activeJobs) > 0 {
		logger.V(1).Info("concurrency policy blocks concurrent runs, skipping", "num active", len(activeJobs))
		return scheduledResult, nil
	}
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ReplaceConcurrent {
		for _, activeJob := range activeJobs {
			if err := r.Delete(ctx, activeJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "unable to delete active job", "job", activeJob)
				return ctrl.Result{}, err
			}
		}
	}
	// 6.4 构造Job
	job, err := r.constructJobForCronJob(&cronJob, missedRun)
	if err != nil {
		logger.Error(err, "unable to construct job from template")
		return scheduledResult, nil
	}
	if err := r.Create(ctx, job); err != nil {
		logger.Error(err, "unable to create job for CronJob", "job", job)
		return ctrl.Result{}, err
	}

	logger.V(1).Info("created Job for cronJob run", "job", job)

	return scheduledResult, nil
}

var (
	jobOwnerKey = ".metadata.controller"
	apiGVStr    = batchv1.GroupVersion.String()
)

func getNextSchedule(cronJob *batchv1.CronJob, now time.Time) (lastMissed time.Time, next time.Time, err error) {
	// ParseStandard
	sched, err := cron.ParseStandard(cronJob.Spec.Schedule)
	if err != nil {
		// 注：%q会自动填充双引号
		return time.Time{}, time.Time{}, fmt.Errorf("unparseable schedule %q: %v", cronJob.Spec.Schedule, err)
	}
	var earliestTime time.Time
	if cronJob.Status.LastScheduleTime != nil {
		earliestTime = cronJob.Status.LastScheduleTime.Time
	} else {
		earliestTime = cronJob.ObjectMeta.CreationTimestamp.Time
	}

	// 这个if条件没懂是什么意思
	if cronJob.Spec.StartingDeadlineSeconds != nil {
		schedulingDeadline := now.Add(-time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds))
		if schedulingDeadline.After(earliestTime) {
			earliestTime = schedulingDeadline
		}
	}

	// 如果上一次的调用时间比现在时间还靠后，那么应该立即规划下一个时间
	if earliestTime.After(now) {
		return time.Time{}, sched.Next(now), nil
	}

	starts := 0
	for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
		lastMissed = t
		// An object might miss several starts. For example, if
		// controller gets wedged on Friday at 5:01pm when everyone has
		// gone home, and someone comes in on Tuesday AM and discovers
		// the problem and restarts the controller, then all the hourly
		// jobs, more than 80 of them for one hourly scheduledJob, should
		// all start running with no further intervention (if the scheduledJob
		// allows concurrency and late starts).
		//
		// However, if there is a bug somewhere, or incorrect clock
		// on controller's server or apiservers (for setting creationTimestamp)
		// then there could be so many missed start times (it could be off
		// by decades or more), that it would eat up all the CPU and memory
		// of this controller. In that case, we want to not try to list
		// all the missed start times.
		starts++
		if starts > 100 {
			// We can't get the most recent times so just return an empty slice
			return time.Time{}, time.Time{}, fmt.Errorf("too many missed start times (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew")
		}
	}

	return lastMissed, sched.Next(now), nil
}

func (r *CronJobReconciler) constructJobForCronJob(cronJob *batchv1.CronJob, scheduledTime time.Time) (*kbatch.Job, error) {
	name := fmt.Sprintf("%s-%d", cronJob.Name, scheduledTime.Unix())
	job := &kbatch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        name,
			Namespace:   cronJob.Namespace,
		},
		Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
	}
	for k, v := range cronJob.Spec.JobTemplate.Annotations {
		job.Annotations[k] = v
	}
	job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	for k, v := range cronJob.Spec.JobTemplate.Labels {
		job.Labels[k] = v
	}
	if err := ctrl.SetControllerReference(cronJob, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil

}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kbatch.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner
		job := rawObj.(*kbatch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		if owner.APIVersion != apiGVStr || owner.Kind != "CronJob" {
			return nil
		}
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.CronJob{}).
		Owns(&kbatch.Job{}).
		Complete(r)
}
