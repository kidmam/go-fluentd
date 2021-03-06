package recvs

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"regexp"
	"time"

	"github.com/Laisky/go-fluentd/libs"
	"github.com/Laisky/go-utils"
	"github.com/Laisky/zap"
	"github.com/kataras/iris"
)

// HTTPRecvCfg is the configuration for HTTPRecv
type HTTPRecvCfg struct {
	HTTPSrv     *iris.Application
	MaxBodySize int64
	// Name: recv name
	// Path: url endpoint
	Name, Path, Env string

	// Tag: set `msg.Tag = Tag`
	// OrigTag & TagKey: set `msg.Message[TagKey] = OrigTag`
	OrigTag, Tag, TagKey, MsgKey string

	// TSRegexp: validate time string
	// TimeKey: load time string from `msg.Message[TimeKey].(string)`
	// TimeFormat: `time.Parse(ts, TimeFormat)`
	TimeKey, TimeFormat string
	TSRegexp            *regexp.Regexp

	// SigKey: load signature from `msg.Message[SigKey].([]byte)`
	// SigSalt: calculate signature by `md5(ts + SigSalt)`
	SigKey  string
	SigSalt []byte

	MaxAllowedDelaySec, MaxAllowedAheadSec time.Duration
}

// HTTPRecv recv for HTTP
type HTTPRecv struct {
	*BaseRecv
	*HTTPRecvCfg
}

// NewHTTPRecv return new HTTPRecv
func NewHTTPRecv(cfg *HTTPRecvCfg) *HTTPRecv {
	utils.Logger.Info("create HTTPRecv",
		zap.String("tag", cfg.Tag),
		zap.String("path", cfg.Path),
		zap.Duration("MaxAllowedAheadSec", cfg.MaxAllowedAheadSec),
		zap.Duration("MaxAllowedDelaySec", cfg.MaxAllowedDelaySec),
	)

	if cfg.Path == "" {
		utils.Logger.Panic("path should not be emqty")
	}

	r := &HTTPRecv{
		BaseRecv:    &BaseRecv{},
		HTTPRecvCfg: cfg,
	}
	r.HTTPSrv.Post(r.Path, r.HTTPLogHandler)
	r.HTTPSrv.Get(r.Path, func(ctx iris.Context) {
		ctx.WriteString("HTTPrecv")
	})
	return r
}

// GetName get current HTTPRecv instance's name
func (r *HTTPRecv) GetName() string {
	return r.Name
}

// Run useless, just capatable for RecvItf
func (r *HTTPRecv) Run() {
	utils.Logger.Info("run HTTPRecv")
}

func (r *HTTPRecv) validate(ctx iris.Context, msg *libs.FluentMsg) bool {
	switch msg.Message[r.TimeKey].(type) {
	case nil:
		utils.Logger.Warn("timekey missed")
		r.BadRequest(ctx, "message should contains "+r.TimeKey)
		return false
	case string:
		msg.Message[r.TimeKey] = []byte(msg.Message[r.TimeKey].(string))
	case []byte:
	default:
		utils.Logger.Warn("unknown type of timekey", zap.String(r.TimeKey, fmt.Sprint(msg.Message[r.TimeKey])))
		r.BadRequest(ctx, "unknown type of timekey")
		return false
	}

	if !r.TSRegexp.Match(msg.Message[r.TimeKey].([]byte)) {
		utils.Logger.Warn("unknown format of timekey", zap.ByteString(r.TimeKey, msg.Message[r.TimeKey].([]byte)))
		r.BadRequest(ctx, "unknown format of timekey")
		return false
	}

	// signature
	switch msg.Message[r.SigKey].(type) {
	case nil:
		utils.Logger.Warn("`sig` not exists")
		r.BadRequest(ctx, "`sig` not exists")
		return false
	case []byte:
	case string:
		msg.Message[r.SigKey] = []byte(msg.Message[r.SigKey].(string))
	default:
		utils.Logger.Warn("`unknown type of `sig`", zap.String(r.SigKey, fmt.Sprint(msg.Message[r.SigKey])))
		r.BadRequest(ctx, "`unknown type of `sig`")
		return false
	}
	hash := md5.Sum(append(msg.Message[r.TimeKey].([]byte), r.SigSalt...))
	sig := hex.EncodeToString(hash[:])
	if sig != string(msg.Message[r.SigKey].([]byte)) {
		utils.Logger.Warn("signature of timekey incorrect",
			zap.String("expect", sig),
			zap.ByteString("got", msg.Message[r.SigKey].([]byte)))
		r.BadRequest(ctx, "signature error")
		return false
	}

	// check whether @timestamp is expires
	now := utils.Clock.GetUTCNow()
	if ts, err := time.Parse(r.TimeFormat, string(msg.Message[r.TimeKey].([]byte))); err != nil {
		utils.Logger.Error("parse ts got error",
			zap.Error(err),
			zap.ByteString(r.TimeKey, msg.Message[r.TimeKey].([]byte)))
		r.BadRequest(ctx, "signature error")
		return false
	} else if now.Sub(ts) > r.MaxAllowedDelaySec {
		utils.Logger.Warn("timekey expires", zap.Time("ts", ts))
		r.BadRequest(ctx, "expires")
		return false
	} else if ts.Sub(now) > r.MaxAllowedAheadSec {
		utils.Logger.Warn("timekey ahead of now",
			zap.Time("ts", ts),
			zap.Time("now", now))
		r.BadRequest(ctx, "come from future?")
		return false
	}

	return true
}

// BadRequest set bad http response
func (r *HTTPRecv) BadRequest(ctx iris.Context, msg string) {
	ctx.StatusCode(iris.StatusBadRequest)
	ctx.WriteString(msg)
}

// HTTPLogHandler process log received by HTTP
func (r *HTTPRecv) HTTPLogHandler(ctx iris.Context) {
	env := ctx.Params().Get("env")
	switch env {
	case "sit":
	case "perf":
	case "uat":
	case "prod":
	default:
		utils.Logger.Warn("unknown env", zap.String("env", env))
		r.BadRequest(ctx, fmt.Sprintf("only accept sit/perf/uat/prod, but got `%v`", env))
		return
	}
	// utils.Logger.Debug("got new http log", zap.String("env", env))

	if ctx.GetContentLength() > r.MaxBodySize {
		utils.Logger.Warn("content size too big", zap.Int64("size", ctx.GetContentLength()))
		r.BadRequest(ctx, fmt.Sprintf("content size must less than %d bytes", r.MaxBodySize))
		return
	}

	msg := r.msgPool.Get().(*libs.FluentMsg)
	if log, err := ioutil.ReadAll(ctx.Request().Body); err != nil {
		utils.Logger.Warn("try to read log got error", zap.Error(err))
		r.msgPool.Put(msg)
		r.BadRequest(ctx, "can not load request body")
		return
	} else {
		msg.Tag = r.Tag + "." + r.Env // forward-xxx.sit
		msg.Message = map[string]interface{}{}
		if err = json.Unmarshal(log, &msg.Message); err != nil {
			utils.Logger.Warn("try to unmarsh json got error")
			r.msgPool.Put(msg)
			r.BadRequest(ctx, "try to unmarsh json body got error")
			return
		}
	}

	if !r.validate(ctx, msg) {
		r.msgPool.Put(msg)
		return
	}

	libs.FlattenMap(msg.Message, "__")
	msg.Message[r.TagKey] = r.OrigTag + "." + env
	msg.Id = r.counter.Count()
	utils.Logger.Debug("receive new msg", zap.String("tag", msg.Tag), zap.Int64("id", msg.Id))
	ctx.Writef(`{"msgid": %d}`, msg.Id)
	r.asyncOutChan <- msg
}
