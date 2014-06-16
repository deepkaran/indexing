package adminport

import (
	c "github.com/couchbase/indexing/secondary/common"
)

var adminportStatsFormat = map[string]interface{}{
	"urlPrefix": "",
	"payload":   []float64{0, 0}, // [request,response] in Bytes
	"requests":  float64(0),      // registered request types
	// for requests.*
}

func (s *httpServer) newStats() *c.ComponentStat {
	statsp, _ := c.NewComponentStat(nil)
	stats := *statsp
	stats["urlPrefix"] = s.urlPrefix
	stats["payload"] = []float64{0, 0}
	stats["requests"] = float64(len(s.messages))
	// for request.*
	for _, msg := range s.messages {
		stats["request."+msg.Name()] = []float64{0, 0, 0}
	}
	return statsp
}
