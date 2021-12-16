package router

import (
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"github.com/toolkits/pkg/ginx"

	"github.com/didi/nightingale/v5/src/server/config"
	"github.com/didi/nightingale/v5/src/server/engine"
	"github.com/didi/nightingale/v5/src/server/idents"
	"github.com/didi/nightingale/v5/src/server/memsto"
	"github.com/didi/nightingale/v5/src/server/reader"
	promstat "github.com/didi/nightingale/v5/src/server/stat"
	"github.com/didi/nightingale/v5/src/server/writer"
)

type promqlForm struct {
	PromQL string `json:"promql"`
}

func queryPromql(c *gin.Context) {
	var f promqlForm
	ginx.BindJSON(c, &f)

	value, warnings, err := reader.Reader.Client.Query(c.Request.Context(), f.PromQL, time.Now())
	if err != nil {
		c.String(500, "promql:%s error:%v", f.PromQL, err)
		return
	}

	if len(warnings) > 0 {
		c.String(500, "promql:%s warnings:%v", f.PromQL, warnings)
		return
	}

	c.JSON(200, engine.ConvertVectors(value))
}

func remoteWrite(c *gin.Context) {
	req, err := DecodeWriteRequest(c.Request.Body)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	count := len(req.Timeseries)

	if count == 0 {
		c.String(200, "")
		return
	}

	var (
		now = time.Now().Unix()
		ids = make(map[string]interface{})
		lst = make([]interface{}, count)
	)

	for i := 0; i < count; i++ {
		labels := req.Timeseries[i].GetLabels()

		ident := ""
		for _, label := range labels {
			if label.GetName() == "ident" {
				ident = label.GetValue()
			}
		}

		if len(ident) > 0 {
			// register host
			ids[ident] = now

			// fill tags
			target, has := memsto.TargetCache.Get(ident)
			if has {
				for key, value := range target.TagsMap {
					req.Timeseries[i].Labels = append(req.Timeseries[i].Labels, &prompb.Label{
						Name:  key,
						Value: value,
					})
				}
			}
		}

		lst[i] = req.Timeseries[i]
	}

	promstat.CounterSampleTotal.WithLabelValues(config.C.ClusterName, "prometheus").Add(float64(count))
	idents.Idents.MSet(ids)
	if writer.Writers.PushQueue(lst) {
		c.String(200, "")
	} else {
		c.String(http.StatusInternalServerError, "wirter queue full")
	}
}

// DecodeWriteRequest from an io.Reader into a prompb.WriteRequest, handling
// snappy decompression.
func DecodeWriteRequest(r io.Reader) (*prompb.WriteRequest, error) {
	compressed, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	reqBuf, err := snappy.Decode(nil, compressed)
	if err != nil {
		return nil, err
	}

	var req prompb.WriteRequest
	if err := proto.Unmarshal(reqBuf, &req); err != nil {
		return nil, err
	}

	return &req, nil
}
