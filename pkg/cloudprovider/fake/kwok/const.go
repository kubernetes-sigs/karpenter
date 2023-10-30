package kwok


const (
	kwokProviderPrefix = "kwok://"
)

var (
	kwokZones = []string{"test-zone-a", "test-zone-b", "test-zone-c", "test-zone-d"}
	kwokPartitions = []string{"partition-a", "partition-b", "partition-c", "partition-d", "partition-e",
		"partition-f", "partition-g", "partition-h", "partition-i", "partition-j"}
)

/*
We care about this part
1. Instance Type Name: (family=(category)-(generation)-(suffix-class))-(size)
	a. size: default, 2, 4, 8, 16, 32, 48, 64, 98, 192
	b. family: c (cpu), g (general), m (memory)
		i. c ---> 1 vCPU : 2 GiB
		ii. g --> 1 vCPU : 4 GiB
		iii. m -> 1 vCPU : 8 GiB
2. Capacity Type: [on-demand, spot]
3. Pricing: function of capacity type, size
	a. On-demand: size * price_of_size_1()
	b. Size: jitter_spot_factor_for_zone() * on-demand_price


1. jitter_spot_factor_for_zone = .8 +/- .05
2. price_of_size_1 = # of vCPU * 0.25 + # of GiB * 0.01
*/
