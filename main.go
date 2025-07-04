package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// 定义Prometheus指标
var (
	// 按namespace和失败类型统计失败Pod数量
	imagePullFailureGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_pod_image_pull_failure_total",
			Help: "Number of pods with image pull failures categorized by namespace and reason",
		},
		[]string{"namespace", "reason"},
	)
)

// podFailures 用于跟踪当前所有失败Pod的状态, key: namespace/pod, value: map[reason]bool
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
	// 计算当前Pod镜像拉取失败的所有原因（可能多个容器多个原因）
	reasons := analyzePodImagePullErrors(pod)

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// 获取旧的原因集合
	oldVal, _ := podFailures.LoadOrStore(key, make(map[string]struct{}))
	oldReasons := oldVal.(map[string]struct{})

	// 对比新旧，更新指标
	updated := false

	// 1. 删除旧的不再存在的原因
	for r := range oldReasons {
		if _, found := reasons[r]; !found {
			// 指标减1
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Dec()
			delete(oldReasons, r)
			updated = true
		}
	}

	// 2. 添加新的原因
	for r := range reasons {
		if _, found := oldReasons[r]; !found {
			imagePullFailureGauge.WithLabelValues(pod.Namespace, r).Inc()
			oldReasons[r] = struct{}{}
			updated = true
		}
	}

	if updated {
		podFailures.Store(key, oldReasons)
	}
}

// onPodDelete 处理Pod删除事件，清理指标
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

// analyzePodImagePullErrors 根据Pod状态分析失败原因，返回map[string]struct{}表征所有失败原因
func analyzePodImagePullErrors(pod *corev1.Pod) map[string]struct{} {
	reasons := make(map[string]struct{})

	// 检查所有容器状态，含init与普通容器
	checkContainerStatuses := func(statuses []corev1.ContainerStatus) {
		for _, cs := range statuses {
			if cs.State.Waiting != nil {
				reason := cs.State.Waiting.Reason
				msg := cs.State.Waiting.Message
				if isImagePullFailureReason(reason) {
					classifiedReason := classifyFailureReason(reason, msg)
					reasons[classifiedReason] = struct{}{}
				}
			}
		}
	}

	checkContainerStatuses(pod.Status.InitContainerStatuses)
	checkContainerStatuses(pod.Status.ContainerStatuses)
	return reasons
}

// isImagePullFailureReason 判断Reason是否为镜像拉取失败相关
func isImagePullFailureReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "Cancelled", "RegistryUnavailable":
		return true
	default:
		return false
	}
}

// classifyFailureReason 根据Reason和详细Message分类失败类型
func classifyFailureReason(reason, message string) string {
	// 优先用Reason简单匹配
	baseReason := strings.ToLower(reason)

	switch baseReason {
	case "errimagepull", "imagepullbackoff":
		lowerMsg := strings.ToLower(message)
		switch {
		case reImageNotFound.MatchString(lowerMsg):
			return "image_not_found"
		case reProxyError.MatchString(lowerMsg):
			return "proxy_error"
		case reUnauthorized.MatchString(lowerMsg):
			return "unauthorized"
		case reTLS.MatchString(lowerMsg):
			return "tls_handshake_error"
		default:
			return "unknown_error"
		}
	default:
		// 其他非标准原因统一归类
		return baseReason
	}
}

func main() {
	// 注册Prometheus指标
	prometheus.MustRegister(imagePullFailureGauge)

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
