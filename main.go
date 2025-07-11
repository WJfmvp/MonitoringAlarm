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
	"sync/atomic"
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
	imagePullFailureGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_pod_image_pull_failure_total",
			Help: "Number of pods with image pull failures categorized by namespace and reason",
		},
		[]string{"namespace", "reason"},
	)
	imagePullFailureAlertCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_pod_image_pull_failure_alerts_total",
			Help: "Total number of image pull failure alerts triggered, by namespace and reason",
		},
		[]string{"namespace", "reason"},
	)
)

// podInfo 包含失败原因及锁
type podInfo struct {
	mu      sync.Mutex
	reasons map[string]struct{}
}

type alertCount struct {
	count atomic.Int64
}

var podFailures sync.Map
var alertCounts sync.Map

// 为不同失败原因预定义正则表达式，用于根据错误信息做归类
var (
	reImageNotFound = regexp.MustCompile(`(?i)not found|manifest unknown|repository does not exist`)
	reProxyError    = regexp.MustCompile(`(?i)proxyconnect|proxy error`)
	reUnauthorized  = regexp.MustCompile(`(?i)unauthorized|authentication required`)
	reTLS           = regexp.MustCompile(`(?i)tls handshake`)
)

func onPodAddOrUpdate(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	log.Printf("Pod handler triggered for %s/%s; phase=%s, containers=%d",
		pod.Namespace, pod.Name, pod.Status.Phase, len(pod.Status.ContainerStatuses))

	reasons := analyzePodImagePullErrors(pod)
	if pod.Status.Phase == corev1.PodPending && len(pod.Status.ContainerStatuses) == 0 {
		reasons["sandbox_create_failure"] = struct{}{}
	}

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	piVal, _ := podFailures.LoadOrStore(key, &podInfo{reasons: make(map[string]struct{})})
	pi := piVal.(*podInfo)

	pi.mu.Lock()
	defer pi.mu.Unlock()

	updateReasons(pi, reasons, pod)
}

func updateReasons(pi *podInfo, reasons map[string]struct{}, pod *corev1.Pod) {
	// 删除旧的原因
	for r := range pi.reasons {
		if _, found := reasons[r]; !found {
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Dec()
			delete(pi.reasons, r)
		}
	}

	// 添加新的原因
	for r := range reasons {
		if _, found := pi.reasons[r]; !found {
			// 更新Gauge
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Inc()
			// 更新Counter
			imagePullFailureAlertCounter.WithLabelValues(pod.Namespace, r).Inc()

			// 本地缓存累加并日志输出
			countKey := fmt.Sprintf("%s/%s", pod.Namespace, r)
			acVal, _ := alertCounts.LoadOrStore(countKey, &alertCount{})
			ac := acVal.(*alertCount)
			newCount := ac.count.Add(1)
			log.Printf("Alert #%d for image pull failure in %s reason=%s", newCount, pod.Namespace, r)

			pi.reasons[r] = struct{}{}
		}
	}
}

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
		pi := reasonsVal.(*podInfo)
		pi.mu.Lock()
		defer pi.mu.Unlock()
		for r := range pi.reasons {
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Dec()
		}
	}
}

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

func isImagePullFailureReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "Cancelled", "RegistryUnavailable":
		return true
	default:
		return false
	}
}

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

	// 创建k8s客户端配置（集群内部使用）
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Error creating in cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes clientset: %v", err)
	}

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
