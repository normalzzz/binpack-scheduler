package main

import (
	"context"
	"os"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/cli"
	resourcehelper "k8s.io/component-helpers/resource"
	klog "k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/pkg/features"
)

const (
	pluginName = "BinPack"

	cpuWeight    int64 = 1
	memoryWeight int64 = 1
)

type BinPack struct{}

// 编译时确认实现了 ScorePlugin 接口。
var _ fwk.ScorePlugin = &BinPack{}

func (p *BinPack) Name() string {
	return pluginName
}

func New(
	_ context.Context,
	_ runtime.Object,
	_ fwk.Handle,
) (fwk.Plugin, error) {
	return &BinPack{}, nil
}

// Score 对一个通过 Filter 的候选节点评分。
func (p *BinPack) Score(
	ctx context.Context,
	_ fwk.CycleState,
	pod *v1.Pod,
	nodeInfo fwk.NodeInfo,
) (int64, *fwk.Status) {
	if nodeInfo.Node() == nil {
		return 0, fwk.NewStatus(fwk.Error, "node information is missing")
	}

	// 1. 计算当前 Pod 的有效 requests。
	// 与该版本 scheduler 的相关 feature gate 保持一致。
	podRequests := resourcehelper.PodRequests(
		pod,
		resourcehelper.PodResourcesOptions{
			UseStatusResources: utilfeature.DefaultFeatureGate.Enabled(
				features.InPlacePodVerticalScaling,
			),
			SkipPodLevelResources: !utilfeature.DefaultFeatureGate.Enabled(
				features.PodLevelResources,
			),
		},
	)

	podCPU := podRequests.Cpu().MilliValue()
	podMemory := podRequests.Memory().Value()

	// 2. 获取节点已有请求量和可分配资源。
	requested := nodeInfo.GetRequested()
	allocatable := nodeInfo.GetAllocatable()

	// 3. 计算把当前 Pod 放入节点后的总请求量。
	totalCPU := requested.GetMilliCPU() + podCPU
	totalMemory := requested.GetMemory() + podMemory

	// 4. 分别计算 CPU 和 Memory 得分。
	cpuScore := mostAllocatedScore(
		totalCPU,
		allocatable.GetMilliCPU(),
	)
	memoryScore := mostAllocatedScore(
		totalMemory,
		allocatable.GetMemory(),
	)

	// 5. 加权平均，得到 0～100 的最终分数。
	score := (cpuScore*cpuWeight + memoryScore*memoryWeight) /
		(cpuWeight + memoryWeight)

	klog.FromContext(ctx).V(4).Info(
		"BinPack score",
		"pod", klog.KObj(pod),
		"node", nodeInfo.Node().Name,
		"cpuRequestedAfter", totalCPU,
		"memoryRequestedAfter", totalMemory,
		"cpuScore", cpuScore,
		"memoryScore", memoryScore,
		"score", score,
	)

	return score, nil
}

// 已经返回 0～100 的分数，不需要额外 NormalizeScore。
func (p *BinPack) ScoreExtensions() fwk.ScoreExtensions {
	return nil
}

// 资源请求占比越高，分数越高。
func mostAllocatedScore(requested, allocatable int64) int64 {
	if allocatable <= 0 || requested <= 0 {
		return 0
	}

	// 防御性处理，确保分数不超过 MaxNodeScore。
	if requested >= allocatable {
		return fwk.MaxNodeScore
	}

	return requested * fwk.MaxNodeScore / allocatable
}

func main() {
	command := app.NewSchedulerCommand(
		app.WithPlugin(pluginName, New),
	)

	os.Exit(cli.Run(command))
}
