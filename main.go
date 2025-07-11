package main

import (
	"fmt"
	"k8s.io/client-go/rest"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// 定义Prometheus指标
var (
	// 按namespace和失败类型统计失败Pod当前数量
	imagePullFailureGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_pod_image_pull_failure_total",
			Help: "Number of pods with image pull failures categorized by namespace and reason",
		},
		[]string{"namespace", "reason"},
	)
	// 按namespace和失败类型统计告警事件发生次数
	imagePullFailureAlertCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_pod_image_pull_failure_alerts_total",
			Help: "Total number of image pull failure alerts triggered, by namespace and reason",
		},
		[]string{"namespace", "reason"},
	)
	// 本地缓存告警次数用于日志
	alertCounts = sync.Map{} // key: "namespace/reason", value: int
)

// podFailures 用于跟踪当前所有失败Pod的状态, key: namespace/pod, value: map[reason]struct{}
var (
	podFailures sync.Map
)

// 为不同失败原因预定义正则表达式，用于根据错误信息做归类
var (
	reImageNotFound = regexp.MustCompile(`(?i)not found|manifest unknown|repository does not exist`)
	reProxyError    = regexp.MustCompile(`(?i)proxyconnect|proxy error`)
	reUnauthorized  = regexp.MustCompile(`(?i)unauthorized|authentication required`)
	reTLS           = regexp.MustCompile(`(?i)tls handshake`)
)

// onPodAddOrUpdate 处理Pod新建/更新事件
func onPodAddOrUpdate(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	// 日志打印
	log.Printf("Pod handler triggered for %s/%s; phase=%s, containers=%d",
		pod.Namespace, pod.Name, pod.Status.Phase, len(pod.Status.ContainerStatuses))

	// 计算当前Pod镜像拉取失败的所有原因
	reasons := analyzePodImagePullErrors(pod)
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// 如果Phase为Pending且没有容器状态，统计sandbox失败
	if pod.Status.Phase == corev1.PodPending && len(pod.Status.ContainerStatuses) == 0 {
		reasons["sandbox_create_failure"] = struct{}{}
	}

	if len(reasons) > 0 {
		log.Printf("Pod %s has image pull failure reasons: %v", key, reasons)
	}

	// 获取旧的原因集合
	oldVal, _ := podFailures.LoadOrStore(key, make(map[string]struct{}))
	oldReasons := oldVal.(map[string]struct{})

	// 删除旧的不再存在的原因
	for r := range oldReasons {
		if _, found := reasons[r]; !found {
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Dec()
			delete(oldReasons, r)
		}
	}

	// 添加新的原因，同时触发告警计数并记录日志
	for r := range reasons {
		if _, found := oldReasons[r]; !found {
			// 更新Gauge
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Inc()
			// 更新Counter
			imagePullFailureAlertCounter.WithLabelValues(pod.Namespace, r).Inc()
			// 本地缓存累加并日志输出
			countKey := fmt.Sprintf("%s/%s", pod.Namespace, r)
			val, _ := alertCounts.LoadOrStore(countKey, 0)
			newCount := val.(int) + 1
			alertCounts.Store(countKey, newCount)
			log.Printf("Alert #%d for image pull failure in %s reason=%s", newCount, pod.Namespace, r)

			oldReasons[r] = struct{}{}
		}
	}

	// 存储更新后的原因集合
	podFailures.Store(key, oldReasons)
}

// onPodDelete 处理Pod删除事件，清理Gauge指标
func onPodDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	reasonsVal, loaded := podFailures.LoadAndDelete(key)
	if loaded {
		reasons := reasonsVal.(map[string]struct{})
		for r := range reasons {
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Dec()
		}
	}
}

// analyzePodImagePullErrors 分析Pod容器状态，返回所有失败原因
func analyzePodImagePullErrors(pod *corev1.Pod) map[string]struct{} {
	reasons := make(map[string]struct{})
	checkContainerStatuses := func(statuses []corev1.ContainerStatus) {
		for _, cs := range statuses {
			if cs.State.Waiting != nil {
				reason := cs.State.Waiting.Reason
				msg := cs.State.Waiting.Message
				if isImagePullFailureReason(reason) {
					classified := classifyFailureReason(reason, msg)
					reasons[classified] = struct{}{}
				}
			}
		}
	}

	checkContainerStatuses(pod.Status.InitContainerStatuses)
	checkContainerStatuses(pod.Status.ContainerStatuses)
	return reasons
}

// isImagePullFailureReason 判断是否为镜像拉取失败相关reason
func isImagePullFailureReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "Cancelled", "RegistryUnavailable":
		return true
	default:
		return false
	}
}

// classifyFailureReason 根据reason和message分类失败类型
func classifyFailureReason(reason, message string) string {
	base := strings.ToLower(reason)
	switch base {
	case "errimagepull", "imagepullbackoff":
		low := strings.ToLower(message)
		switch {
		case reImageNotFound.MatchString(low):
			return "image_not_found"
		case reProxyError.MatchString(low):
			return "proxy_error"
		case reUnauthorized.MatchString(low):
			return "unauthorized"
		case reTLS.MatchString(low):
			return "tls_handshake_error"
		default:
			log.Printf("Classify default for %s: %s", reason, message)
			return "unknown_error"
		}
	default:
		return base
	}
}

func main() {
	// 注册Prometheus指标
	prometheus.MustRegister(imagePullFailureGauge)
	prometheus.MustRegister(imagePullFailureAlertCounter)

	//// 创建k8s客户端配置（集群内部使用）
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Error creating in cluster config: %v", err)
	}

	// 创建k8s客户端配置，使用本地的Kubeconfig文件
	//config, err := clientcmd.BuildConfigFromFlags("", "/Users/wj/kube/local-test")
	//if err != nil {
	//	log.Fatalf("Error loading kubeconfig: %v", err)
	//}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes clientset: %v", err)
	}

	//imagePullFailureGauge.WithLabelValues("debug-ns", "debug-reason").Set(0)

	// 创建SharedInformerFactory，监听所有命名空间的Pod事件
	factory := informers.NewSharedInformerFactory(clientset, 0)
	podInformer := factory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    onPodAddOrUpdate,
		UpdateFunc: func(oldObj, newObj interface{}) { onPodAddOrUpdate(newObj) },
		DeleteFunc: onPodDelete,
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	// 启动Informer
	go podInformer.Run(stopCh)

	// 等待缓存同步
	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced) {
		log.Fatalf("Timed out waiting for caches to sync")
	}

	// 启动HTTP服务器，暴露metrics接口
	http.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Println("Starting metrics server at :8080")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatalf("Error starting HTTP server: %v", err)
		}
	}()

	// 优雅退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutdown signal received, exiting...")
}
