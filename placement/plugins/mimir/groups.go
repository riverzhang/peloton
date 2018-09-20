package mimir

import (
	"fmt"
	"strings"

	"code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"code.uber.internal/infra/peloton/mimir-lib/model/labels"
	"code.uber.internal/infra/peloton/mimir-lib/model/metrics"
	"code.uber.internal/infra/peloton/mimir-lib/model/placement"
)

// OfferToGroup will convert an offer to a group.
func OfferToGroup(hostOffer *hostsvc.HostOffer) *placement.Group {
	group := placement.NewGroup(hostOffer.Hostname)
	group.Metrics = makeMetrics(hostOffer.GetResources())
	group.Labels = makeLabels(hostOffer.Attributes)
	return group
}

func makeMetrics(resources []*mesos_v1.Resource) *metrics.Set {
	result := metrics.NewSet()
	for _, resource := range resources {
		value := resource.GetScalar().GetValue()
		switch name := resource.GetName(); name {
		case "cpus":
			result.Add(CPUAvailable, value*100.0)
			result.Set(CPUFree, 0.0)
		case "gpus":
			result.Add(GPUAvailable, value*100.0)
			result.Set(GPUFree, 0.0)
		case "mem":
			result.Add(MemoryAvailable, value*metrics.MiB)
			result.Set(MemoryFree, 0.0)
		case "disk":
			result.Add(DiskAvailable, value*metrics.MiB)
			result.Set(DiskFree, 0.0)
		case "ports":
			ports := uint64(0)
			for _, r := range resource.GetRanges().GetRange() {
				ports += r.GetEnd() - r.GetBegin() + 1
			}
			result.Add(PortsAvailable, float64(ports))
			result.Set(PortsFree, 0.0)
		}
	}
	// Compute the derived metrics, e.g. the free metrics from the available and reserved metrics.
	result.Update()
	return result
}

// makeLabels will convert Mesos attributes into Mimir labels.
// A scalar attribute with name n and value v will be turned into the label ["n", "v"].
// A text attribute with name n and value t will be turned into the label ["n", "t"].
// A ranges attribute with name n and ranges [r_1a:r_1b], ..., [r_na:r_nb] will be turned into
// the label ["n", "[r_1a-r1b];...[r_na-r_nb]"].
func makeLabels(attributes []*mesos_v1.Attribute) *labels.Bag {
	result := labels.NewBag()
	for _, attribute := range attributes {
		var value string
		switch attribute.GetType() {
		case mesos_v1.Value_SCALAR:
			value = fmt.Sprintf("%v", attribute.GetScalar().GetValue())
		case mesos_v1.Value_TEXT:
			value = attribute.GetText().GetValue()
		case mesos_v1.Value_RANGES:
			ranges := attribute.GetRanges().GetRange()
			length := len(ranges)
			for i, valueRange := range ranges {
				value += fmt.Sprintf("[%v-%v]", valueRange.GetBegin(), valueRange.GetEnd())
				if i < length-1 {
					value += ";"
				}
			}
		}
		names := strings.Split(attribute.GetName(), ".")
		names = append(names, value)
		result.Add(labels.NewLabel(names...))
	}
	return result
}
