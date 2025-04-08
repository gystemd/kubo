package node

import (
	"context"
	"time"

	"github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/bitswap/client"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	blockstore "github.com/ipfs/boxo/blockstore"
	exchange "github.com/ipfs/boxo/exchange"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/exchange/providing"
	provider "github.com/ipfs/boxo/provider"
	rpqm "github.com/ipfs/boxo/routing/providerquerymanager"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/kubo/config"
	irouting "github.com/ipfs/kubo/routing"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/routing"
	"go.uber.org/fx"

	"github.com/ipfs/kubo/core/node/helpers"
)

var log = logging.Logger("core/node/bitswap")

// Docs: https://github.com/ipfs/kubo/blob/master/docs/config.md#internalbitswap
const (
	DefaultEngineBlockstoreWorkerCount = 128
	DefaultTaskWorkerCount             = 8
	DefaultEngineTaskWorkerCount       = 8
	DefaultMaxOutstandingBytesPerPeer  = 1 << 20
	DefaultProviderSearchDelay         = 1000 * time.Millisecond
	DefaultWantHaveReplaceSize         = 1024
)

type bitswapOptionsOut struct {
	fx.Out

	BitswapOpts []bitswap.Option `group:"bitswap-options,flatten"`
}

// BitswapOptions creates configuration options for Bitswap from the config file
// and whether to provide data.
func BitswapOptions(cfg *config.Config) interface{} {
	return func() bitswapOptionsOut {
		var internalBsCfg config.InternalBitswap
		if cfg.Internal.Bitswap != nil {
			internalBsCfg = *cfg.Internal.Bitswap
		}

		opts := []bitswap.Option{
			bitswap.ProviderSearchDelay(internalBsCfg.ProviderSearchDelay.WithDefault(DefaultProviderSearchDelay)), // See https://github.com/ipfs/go-ipfs/issues/8807 for rationale
			bitswap.EngineBlockstoreWorkerCount(int(internalBsCfg.EngineBlockstoreWorkerCount.WithDefault(DefaultEngineBlockstoreWorkerCount))),
			bitswap.TaskWorkerCount(int(internalBsCfg.TaskWorkerCount.WithDefault(DefaultTaskWorkerCount))),
			bitswap.EngineTaskWorkerCount(int(internalBsCfg.EngineTaskWorkerCount.WithDefault(DefaultEngineTaskWorkerCount))),
			bitswap.MaxOutstandingBytesPerPeer(int(internalBsCfg.MaxOutstandingBytesPerPeer.WithDefault(DefaultMaxOutstandingBytesPerPeer))),
			bitswap.WithWantHaveReplaceSize(int(internalBsCfg.WantHaveReplaceSize.WithDefault(DefaultWantHaveReplaceSize))),
		}

		return bitswapOptionsOut{BitswapOpts: opts}
	}
}

type bitswapIn struct {
	fx.In

	Mctx        helpers.MetricsCtx
	Cfg         *config.Config
	Host        host.Host
	Rt          irouting.ProvideManyRouter
	Bs          blockstore.GCBlockstore
	BitswapOpts []bitswap.Option `group:"bitswap-options"`
}

// Bitswap creates the BitSwap server/client instance.
// Additional options to bitswap.New can be provided via the "bitswap-options"
// group.
// If config.Bitswap.Enabled is set to false, this returns nil.
func Bitswap(provide bool) interface{} {
	return func(in bitswapIn, lc fx.Lifecycle) (*bitswap.Bitswap, error) {
		// Check top-level Enabled flag. Note: Flag(0) means default (true).
		bitswapEnabled := in.Cfg.Bitswap.Enabled != config.Flag(-1)
		if !bitswapEnabled {
			log.Info("Bitswap is disabled via Bitswap.Enabled=false")
			// Return nil to indicate Bitswap is not available
			// Fx wiring should handle this by not providing *bitswap.Bitswap
			// to components that require it, or by providing an alternative.
			// OnlineExchange handles providing an offline exchange.
			return nil, nil
		}

		log.Info("Bitswap is enabled")
		bitswapNetwork := bsnet.NewFromIpfsHost(in.Host)

		var provider routing.ContentDiscovery
		// Client provider discovery logic: Use rpqm only if reprovider strategy allows (provide=true)
		// This is independent of the ServerEnabled flag, allowing client functionality even if server is off.
		if provide {
			log.Debug("Bitswap client provider discovery enabled (reprovider strategy allows)")
			// We need to hardcode the default because it is an
			// internal setting in boxo.
			pqm, err := rpqm.New(bitswapNetwork,
				in.Rt,
				rpqm.WithMaxProviders(10),
				rpqm.WithIgnoreProviders(in.Cfg.Routing.IgnoreProviders...),
			)
			if err != nil {
				return nil, err
			}
			in.BitswapOpts = append(in.BitswapOpts, bitswap.WithClientOption(client.WithDefaultProviderQueryManager(false)))
			provider = pqm
		} else {
			log.Debug("Bitswap client provider discovery disabled (reprovider strategy does not allow)")
		}

		// Create the Bitswap instance
		bs := bitswap.New(helpers.LifecycleCtx(in.Mctx, lc), bitswapNetwork, provider, in.Bs, in.BitswapOpts...)

		lc.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				return bs.Close()
			},
		})
		return bs, nil
	}
}

type onlineExchangeIn struct {
	fx.In

	// Use fx.Optional to handle the case where Bitswap is disabled
	BitswapInstance *bitswap.Bitswap `optional:"true"`
	BsBlockstore    blockstore.GCBlockstore
	Cfg             *config.Config
	Lc              fx.Lifecycle
}

// OnlineExchange creates new LibP2P backed block exchange.
// If Bitswap is disabled via config (Bitswap.Enabled=false), it returns an offline exchange.
func OnlineExchange() interface{} {
	return func(in onlineExchangeIn) exchange.Interface {
		// Check top-level Enabled flag. Note: Flag(0) means default (true).
		bitswapEnabled := in.Cfg.Bitswap.Enabled != config.Flag(-1)

		if !bitswapEnabled {
			log.Info("Bitswap is disabled (Bitswap.Enabled=false), using offline exchange")
			// Return offline exchange if bitswap is disabled
			return offline.Exchange(in.BsBlockstore)
		}

		// If Bitswap is enabled, BitswapInstance should be non-nil,
		// unless Bitswap() constructor failed, which fx would handle.
		// Add a defensive check just in case.
		if in.BitswapInstance == nil {
			log.Error("Bitswap is enabled but Bitswap instance is nil, falling back to offline exchange")
			return offline.Exchange(in.BsBlockstore)
		}

		log.Info("Bitswap is enabled, using Bitswap as the exchange")
		// Bitswap is enabled, return the Bitswap instance
		// The lifecycle hook for bs.Close() is already added in the Bitswap constructor
		return in.BitswapInstance
	}
}

type providingExchangeIn struct {
	fx.In

	BaseExch exchange.Interface
	Provider provider.System
	Cfg      *config.Config
	Lc       fx.Lifecycle
}

// ProvidingExchange creates a providing.Exchange with the existing exchange
// and the provider.System.
// This wrapper is only added if Bitswap is enabled AND Bitswap server is enabled
// AND the reprovider strategy allows providing.
// We cannot do this in OnlineExchange because it causes cycles so this is for
// a decorator.
func ProvidingExchange(provide bool /* reflects reprovider strategy */) interface{} {
	return func(in providingExchangeIn) exchange.Interface {
		// Check top-level Enabled flag. Note: Flag(0) means default (true).
		bitswapEnabled := in.Cfg.Bitswap.Enabled != config.Flag(-1)

		// If Bitswap itself is disabled, the BaseExch is already an offline exchange.
		// No need to wrap it with providing.
		if !bitswapEnabled {
			log.Debug("ProvidingExchange: Bitswap disabled, returning base exchange (offline)")
			return in.BaseExch
		}

		// Check ServerEnabled flag. Note: Flag(0) means default (true).
		serverEnabled := in.Cfg.Bitswap.ServerEnabled != config.Flag(-1)

		// Determine if we should actually provide based on strategy AND config flags
		shouldProvide := provide && serverEnabled

		exch := in.BaseExch
		if shouldProvide {
			log.Info("ProvidingExchange: Bitswap enabled, Server enabled, and reprovider strategy allows. Wrapping exchange with providing.")
			exch = providing.New(in.BaseExch, in.Provider)
			// Note: The lifecycle hook for the base exchange (Bitswap) is managed elsewhere.
			// We only need to manage the lifecycle of the providing wrapper if we create it.
			// However, providing.New doesn't seem to have a Close() method itself,
			// it just wraps the underlying exchange. So no extra hook needed here.
			// Let's double check boxo/exchange/providing/providing.go...
			// It seems providing.Exchange does have a Close method. Add the hook.
			in.Lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					log.Debug("Closing providing exchange wrapper")
					return exch.Close()
				},
			})
		} else {
			if !provide {
				log.Info("ProvidingExchange: Not wrapping with providing because reprovider strategy is disabled.")
			}
			if !serverEnabled {
				log.Info("ProvidingExchange: Not wrapping with providing because Bitswap.ServerEnabled=false.")
			}
		}
		return exch
	}
}
