package json_rpc

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/forta-network/forta-node/clients/botauth"
	"github.com/forta-network/forta-node/clients/ratelimiter"
	"github.com/rs/cors"

	"github.com/forta-network/forta-core-go/clients/health"
	"github.com/forta-network/forta-core-go/ethereum"
	"github.com/forta-network/forta-core-go/protocol"
	"github.com/forta-network/forta-core-go/protocol/settings"
	"github.com/forta-network/forta-core-go/utils"
	"github.com/forta-network/forta-node/clients/messaging"
	"github.com/forta-network/forta-node/config"
	"github.com/forta-network/forta-node/metrics"
)

// JsonRpcProxy proxies requests from agents to json-rpc endpoint
type JsonRpcProxy struct {
	ctx    context.Context
	cfg    config.JsonRpcConfig
	server *http.Server

	rateLimiter *ratelimiter.RateLimiter

	lastErr          health.ErrorTracker
	botAuthenticator *botauth.BotAuthenticator
}

func (p *JsonRpcProxy) Start() error {
	p.botAuthenticator.RegisterMessageHandlers()

	rpcUrl, err := url.Parse(p.cfg.Url)
	if err != nil {
		return err
	}
	rp := httputil.NewSingleHostReverseProxy(rpcUrl)

	d := rp.Director
	rp.Director = func(r *http.Request) {
		d(r)
		r.Host = rpcUrl.Host
		r.URL = rpcUrl
		for h, v := range p.cfg.Headers {
			r.Header.Set(h, v)
		}
	}

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	})

	p.server = &http.Server{
		Addr:    ":8545",
		Handler: p.metricHandler(c.Handler(rp)),
	}
	utils.GoListenAndServe(p.server)
	return nil
}

func (p *JsonRpcProxy) metricHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t := time.Now()
		agentConfig, foundAgent := p.botAuthenticator.FindAgentFromRemoteAddr(req.RemoteAddr)
		if foundAgent && p.rateLimiter.ExceedsLimit(agentConfig.ID) {
			writeTooManyReqsErr(w, req)
			p.botAuthenticator.MsgClient().PublishProto(
				messaging.SubjectMetricAgent, &protocol.AgentMetricList{
					Metrics: metrics.GetJSONRPCMetrics(*agentConfig, t, 0, 1, 0),
				},
			)
			return
		}

		h.ServeHTTP(w, req)

		if foundAgent {
			duration := time.Since(t)
			p.botAuthenticator.MsgClient().PublishProto(
				messaging.SubjectMetricAgent, &protocol.AgentMetricList{
					Metrics: metrics.GetJSONRPCMetrics(*agentConfig, t, 1, 0, duration),
				},
			)
		}
	})
}

func (p *JsonRpcProxy) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

func (p *JsonRpcProxy) Name() string {
	return "json-rpc-proxy"
}

// Health implements health.Reporter interface.
func (p *JsonRpcProxy) Health() health.Reports {
	return health.Reports{
		p.lastErr.GetReport("api"),
	}
}

func (p *JsonRpcProxy) apiHealthChecker() {
	p.testAPI()
	ticker := time.NewTicker(time.Minute * 5)
	for range ticker.C {
		p.testAPI()
	}
}

func (p *JsonRpcProxy) testAPI() {
	err := ethereum.TestAPI(p.ctx, "http://localhost:8545")
	p.lastErr.Set(err)
}

func NewJsonRpcProxy(ctx context.Context, cfg config.Config) (*JsonRpcProxy, error) {
	jCfg := cfg.Scan.JsonRpc
	if len(cfg.JsonRpcProxy.JsonRpc.Url) > 0 {
		jCfg = cfg.JsonRpcProxy.JsonRpc
	}

	rateLimiting := cfg.JsonRpcProxy.RateLimitConfig
	if rateLimiting == nil {
		rateLimiting = (*config.RateLimitConfig)(settings.GetChainSettings(cfg.ChainID).JsonRpcRateLimiting)
	}

	botAuthenticator, err := botauth.NewBotAuthenticator(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &JsonRpcProxy{
		ctx:              ctx,
		cfg:              jCfg,
		botAuthenticator: botAuthenticator,
		rateLimiter: ratelimiter.NewRateLimiter(
			rateLimiting.Rate,
			rateLimiting.Burst,
		),
	}, nil
}
