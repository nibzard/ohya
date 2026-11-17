package observe

import (
	"runtime"
	"time"
)

func SampleRuntime(rec *Recorder) {
	start := time.Now()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rec.Resource(ResourceHeapAllocBytes, Supported(float64(ms.HeapAlloc)))
	rec.Resource(ResourceHeapInuseBytes, Supported(float64(ms.HeapInuse)))
	rec.Resource(ResourceHeapSysBytes, Supported(float64(ms.HeapSys)))
	rec.Resource(ResourceGoroutines, Supported(float64(runtime.NumGoroutine())))
	rec.Resource(ResourceGCCount, Supported(float64(ms.NumGC)))
	rec.Resource(ResourceGCCPausMS, Supported(float64(ms.PauseTotalNs)/float64(time.Millisecond)))
	if cpu, ok := SelfCPUSeconds(); ok {
		rec.Resource(ResourceCPUSeconds, Supported(cpu))
	} else {
		rec.Resource(ResourceCPUSeconds, Unsupported("process cpu seconds are unavailable on this platform"))
		rec.Add("runtime_sample_errors", 1)
	}
	if rss, ok := SelfRSSBytes(); ok {
		rec.Resource(ResourceRSSBytes, Supported(rss))
	} else {
		rec.Resource(ResourceRSSBytes, Unsupported("resident set size is unavailable on this platform"))
	}
	rec.Observe("runtime_sample_duration", time.Since(start))
}
