// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package gasprice

import (
	"context"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
)

// Number of transactions sampled in a block
const sampleNumber = 3

var (
	DefaultMinPrioFee    = big.NewInt(1 * params.GWei)
	DefaultMaxPrioFee    = big.NewInt(500 * params.GWei)
	DefaultIgnorePrioFee = big.NewInt(2 * params.Wei)
)

type Config struct {
	Blocks           int
	Percentile       int
	MaxHeaderHistory int
	MaxBlockHistory  int
	Default          *big.Int `toml:",omitempty"`
	MaxPrice         *big.Int `toml:",omitempty"`
	IgnorePrice      *big.Int `toml:",omitempty"`
}

// OracleBackend includes all necessary background APIs for oracle.
type OracleBackend interface {
	HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error)
	PendingBlockAndReceipts() (*types.Block, types.Receipts)
	ChainConfig() *params.ChainConfig
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
}

type baseFeeCacheResut struct {
	baseFee *big.Int
}

type tipFeeCacheEntry struct {
	blockNumber uint64
	fee         *big.Int
}

type tipFeeCache struct {
	// tipFeeCache
	num   int
	cache []tipFeeCacheEntry
}

type fees struct {
	// Lock for headValue
	lock sync.RWMutex

	// latest evaluated block
	tipFee *big.Int

	// cache of tip fees
	tipCache tipFeeCache
}

// Oracle recommends gas prices based on the content of recent
// blocks. Suitable for both light and full clients.
type Oracle struct {
	backend OracleBackend
	fees    fees

	maxPrioFee    *big.Int
	ignorePrioFee *big.Int

	checkBlocks, percentile int

	// FeeHistory
	maxHeaderHistory int
	maxBlockHistory  int
	historyCache     *lru.Cache
	baseFeeCache     *lru.Cache
}

// NewOracle returns a new gasprice oracle which can recommend suitable
// gasprice for newly created transaction.
func NewOracle(backend OracleBackend, params Config) *Oracle {
	blocks := params.Blocks
	if blocks < 1 {
		blocks = 1
		log.Warn("Sanitizing invalid gasprice oracle sample blocks", "provided", params.Blocks, "updated", blocks)
	}
	percent := params.Percentile
	if percent < 0 {
		percent = 0
		log.Warn("Sanitizing invalid gasprice oracle sample percentile", "provided", params.Percentile, "updated", percent)
	} else if percent > 100 {
		percent = 100
		log.Warn("Sanitizing invalid gasprice oracle sample percentile", "provided", params.Percentile, "updated", percent)
	}
	maxTipPrice := params.MaxPrice
	if maxTipPrice == nil || maxTipPrice.Int64() <= 0 {
		maxTipPrice = DefaultMaxPrioFee
		log.Warn("Sanitizing invalid gasprice oracle price cap", "provided", params.MaxPrice, "updated", maxTipPrice)
	}
	ignoreTipPrice := params.IgnorePrice
	if ignoreTipPrice == nil || ignoreTipPrice.Int64() <= 0 {
		ignoreTipPrice = DefaultIgnorePrioFee
		log.Warn("Sanitizing invalid gasprice oracle ignore price", "provided", params.IgnorePrice, "updated", ignoreTipPrice)
	} else if ignoreTipPrice.Int64() > 0 {
		log.Info("Gasprice oracle is ignoring threshold set", "threshold", ignoreTipPrice)
	}
	maxHeaderHistory := params.MaxHeaderHistory
	if maxHeaderHistory < 1 {
		maxHeaderHistory = 1
		log.Warn("Sanitizing invalid gasprice oracle max header history", "provided", params.MaxHeaderHistory, "updated", maxHeaderHistory)
	}
	maxBlockHistory := params.MaxBlockHistory
	if maxBlockHistory < 1 {
		maxBlockHistory = 1
		log.Warn("Sanitizing invalid gasprice oracle max block history", "provided", params.MaxBlockHistory, "updated", maxBlockHistory)
	}

	oracle := &Oracle{
		backend:          backend,
		maxPrioFee:       maxTipPrice,
		ignorePrioFee:    ignoreTipPrice,
		checkBlocks:      blocks,
		percentile:       percent,
		maxHeaderHistory: maxHeaderHistory,
		maxBlockHistory:  maxBlockHistory,
	}

	oracle.historyCache, _ = lru.New(2048)
	oracle.baseFeeCache, _ = lru.New(128)
	oracle.fees.tipCache.cache = make([]tipFeeCacheEntry, blocks*sampleNumber)

	oracle.fees.tipFee = DefaultMinPrioFee

	headEvent := make(chan core.ChainHeadEvent, 1)
	backend.SubscribeChainHeadEvent(headEvent)
	go func() {
		var lastHead common.Hash
		for ev := range headEvent {
			oracle.handleHeadEvent(ev.Block, ev.Block.ParentHash() == lastHead)
			lastHead = ev.Block.Hash()
		}
	}()

	return oracle
}

func (gpo *Oracle) handleHeadEvent(block *types.Block, contiguous bool) {
	if !contiguous {
		gpo.historyCache.Purge()
	}

	tipFee := gpo.calculateTipFee(block)

	gpo.fees.lock.Lock()
	defer gpo.fees.lock.Unlock()

	gpo.fees.tipFee = tipFee
}

// SuggestTipCap returns a tip cap so that newly created transaction can have a
// very high chance to be included in the following blocks.
//
// Note, for legacy transactions and the legacy eth_gasPrice RPC call, it will be
// necessary to add the basefee to the returned number to fall back to the legacy
// behavior.
func (gpo *Oracle) SuggestTipCap(ctx context.Context) (*big.Int, error) {
	gpo.fees.lock.RLock()
	defer gpo.fees.lock.RUnlock()

	return new(big.Int).Set(gpo.fees.tipFee), nil
}

// tip fee cache sorter sorts in ascending order
func (c tipFeeCache) Len() int           { return c.num }
func (c tipFeeCache) Swap(i, j int)      { c.cache[i], c.cache[j] = c.cache[j], c.cache[i] }
func (c tipFeeCache) Less(i, j int) bool { return c.cache[i].fee.Cmp(c.cache[j].fee) < 0 }

// calculateTipFee updates tipCache by removing outdated blocks and adding
// up to tipFeeHistorySamples (cheapest) new elements.
// If we have enough data, we get a good candiate by selecting the
// percentile position in our tipCache.
func (gpo *Oracle) calculateTipFee(block *types.Block) *big.Int {
	// remove all entries older than tipFeeHistoryBlocks
	// also make sure that accidential future blocks are removed
	cache := &gpo.fees.tipCache
	signer := types.MakeSigner(gpo.backend.ChainConfig(), block.Number())

	if block.Number().Uint64() >= uint64(gpo.checkBlocks) {
		removal := block.Number().Uint64() - uint64(gpo.checkBlocks)
		nextInsertPos := 0
		for i := 0; i < cache.num; i++ {
			if cache.cache[i].blockNumber > removal &&
				cache.cache[i].blockNumber < block.Number().Uint64() {

				if i != nextInsertPos {
					cache.cache[nextInsertPos] = cache.cache[i]
				}
				nextInsertPos++
			}
		}
		cache.num = nextInsertPos
	}

	// get max of tipFeeHistorySamples out of the new block
	numResults := 0
	var results [sampleNumber]tipFeeCacheEntry

	for _, tx := range block.Transactions() {
		// don't use coinbase transactions for calculation
		sender, err := types.Sender(signer, tx)
		if err != nil || sender == block.Coinbase() {
			continue
		}
		if tipFee, err := tx.EffectiveGasTip(block.BaseFee()); err == nil {
			if err != nil || (gpo.ignorePrioFee != nil &&
				tipFee.Cmp(gpo.ignorePrioFee) == -1) {
				continue
			}
			insertPos := -1
			if numResults < sampleNumber {
				insertPos = numResults
				numResults++
			} else {
				for i := 0; i < sampleNumber; i++ {
					if results[i].fee.Cmp(tipFee) > 0 && (insertPos < 0 ||
						results[i].fee.Cmp(results[insertPos].fee) > 0) {
						insertPos = i
					}
				}
			}
			if insertPos >= 0 {
				results[insertPos] = tipFeeCacheEntry{blockNumber: block.Number().Uint64(), fee: tipFee}
			}
		}
	}
	if numResults > 0 {
		for numResults > 0 {
			numResults--
			cache.cache[cache.num] = results[numResults]
			cache.num++
		}
		sort.Sort(cache)
	}
	if cache.num >= gpo.checkBlocks {
		tipFee := cache.cache[(cache.num*gpo.percentile)/100].fee
		if tipFee.Cmp(DefaultMinPrioFee) < 0 {
			tipFee = DefaultMinPrioFee
		} else if tipFee.Cmp(gpo.maxPrioFee) > 0 {
			tipFee = gpo.maxPrioFee
		}
		return tipFee
	}
	return gpo.fees.tipFee
}
