package network

import "testing"

// what tc prints for a cake queue (kernel 7.0, iproute2 6.x), shortened
const cakeJSON = `[{"kind":"cake","handle":"8004:","root":true,"refcnt":2,"options":{"bandwidth":2500000,"diffserv":"diffserv3","flowmode":"dual-dsthost","nat":true,"wash":true,"ingress":true,"overhead":18,"mpu":64},
"bytes":123456,"packets":100,"drops":7,"overlimits":0,"requeues":0,"backlog":1500,"qlen":1,"memory_used":0,
"tins":[{"threshold_rate":156250,"avg_delay_us":1200,"peak_delay_us":4000,"drops":3},{"threshold_rate":2500000,"avg_delay_us":3400,"peak_delay_us":9000,"drops":4},{"avg_delay_us":50,"peak_delay_us":70}]}]`

func TestACakeQueueIsReadFromTcsJSON(t *testing.T) {
	d, ok := ParseSQMQdisc([]byte(cakeJSON))
	if !ok || !d.Active || d.BandwidthKbit != 20000 || d.BytesSent != 123456 || d.Drops != 7 || d.BacklogBytes != 1500 || d.AvgDelayUS != 3400 || d.PeakDelayUS != 9000 {
		t.Errorf("%v %+v", ok, d)
	}
}

func TestADeviceWithoutACakeQueueIsNotShaped(t *testing.T) {
	for name, in := range map[string]string{
		"fq_codel": `[{"kind":"fq_codel","handle":"0:","root":true,"options":{}}]`,
		"ingress":  `[{"kind":"ingress","handle":"ffff:","parent":"ffff:fff1","options":{}}]`,
		"nonsense": `not json`,
		"empty":    `[]`,
	} {
		if d, ok := ParseSQMQdisc([]byte(in)); ok || d.Active {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

func TestALineDividedBetweenNetworksIsReadAsTheSumOfItsQueues(t *testing.T) {
	const tree = `[{"kind":"htb","handle":"1:","root":true,"options":{"r2q":10},"bytes":5000,"packets":10,"drops":0,"backlog":0},
{"kind":"cake","handle":"2:","parent":"1:2","options":{"bandwidth":0},"bytes":100,"drops":2,"backlog":300,"tins":[{"avg_delay_us":800,"peak_delay_us":2000}]},
{"kind":"cake","handle":"11:","parent":"1:11","options":{"bandwidth":0},"bytes":4900,"drops":5,"backlog":1200,"tins":[{"avg_delay_us":2500,"peak_delay_us":7000}]}]`
	d, ok := ParseSQMQdisc([]byte(tree))
	if !ok || !d.Active || d.BytesSent != 5000 || d.Drops != 7 || d.BacklogBytes != 1500 || d.AvgDelayUS != 2500 || d.PeakDelayUS != 7000 || d.BandwidthKbit != 0 {
		t.Errorf("%v %+v", ok, d)
	}
}
