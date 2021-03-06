// Copyright 2017 The github.com/blockchain-analysis-study/go-ethereum-analysis Authors
// This file is part of the github.com/blockchain-analysis-study/go-ethereum-analysis library.
//
// The github.com/blockchain-analysis-study/go-ethereum-analysis library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The github.com/blockchain-analysis-study/go-ethereum-analysis library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the github.com/blockchain-analysis-study/go-ethereum-analysis library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"fmt"
	"sync"

	"github.com/blockchain-analysis-study/go-ethereum-analysis/common"
	"github.com/blockchain-analysis-study/go-ethereum-analysis/ethdb"
	"github.com/blockchain-analysis-study/go-ethereum-analysis/trie"
	lru "github.com/hashicorp/golang-lru"
)

// Trie cache generation limit after which to evict trie nodes from memory.   ·Trie· 缓存生成限制，之后将 对应的 trie nodes 从内存中逐出
var MaxTrieCacheGen = uint16(120)

const (
	// Number of past tries to keep. This value is chosen such that
	// reasonable chain reorg depths will hit an existing trie.
	maxPastTries = 12

	// Number of codehash->size associations to keep.
	codeSizeCacheSize = 100000
)

// Database wraps access to tries and contract code.
type Database interface {
	// OpenTrie opens the main account trie.
	OpenTrie(root common.Hash) (Trie, error)

	// OpenStorageTrie opens the storage trie of an account.
	OpenStorageTrie(addrHash, root common.Hash) (Trie, error)

	// CopyTrie returns an independent copy of the given trie.
	CopyTrie(Trie) Trie

	// ContractCode retrieves a particular contract's code.
	ContractCode(addrHash, codeHash common.Hash) ([]byte, error)

	// ContractCodeSize retrieves a particular contracts code's size.
	ContractCodeSize(addrHash, codeHash common.Hash) (int, error)

	// TrieDB retrieves the low level trie database used for data storage.
	TrieDB() *trie.Database
}

// Trie is a Ethereum Merkle Trie.
type Trie interface {
	TryGet(key []byte) ([]byte, error)
	TryUpdate(key, value []byte) error
	TryDelete(key []byte) error
	Commit(onleaf trie.LeafCallback) (common.Hash, error)
	Hash() common.Hash
	NodeIterator(startKey []byte) trie.NodeIterator
	GetKey([]byte) []byte // TODO(fjl): remove this when SecureTrie is removed
	Prove(key []byte, fromLevel uint, proofDb ethdb.Putter) error
}

// NewDatabase creates a backing store for state. The returned database is safe for
// concurrent use and retains cached trie nodes in memory. The pool is an optional
// intermediate trie-node memory pool between the low level storage layer and the
// high level trie abstraction.
/**
对 db 的封装
 */
func NewDatabase(db ethdb.Database) Database {
	/** 封装了 10 W 字节的 lru缓存 */
	csc, _ := lru.New(codeSizeCacheSize)  // 10W 大小的 lru 缓存, 用来存储 codeHash 和code 的
	return &cachingDB{  // todo 这个 cachingDB 最终会被各个StateDB 引用着 ...
		db:            trie.NewDatabase(db),
		// 存放 code 的缓存
		codeSizeCache: csc,
	}
}

// cachingDB 中 也有 SecureTrie 数组  和  LRU 缓存(存放codeHash和code的)
type cachingDB struct {
	db            *trie.Database
	mu            sync.Mutex
	pastTries     []*trie.SecureTrie  // 这里装的是 各个 版本的 StateDB Trie <StateDB 的Trie是 cachedTire 但是最终也是一颗 SecureTrie>
	codeSizeCache *lru.Cache // LRU 缓存(存放codeHash和code的)
}

// OpenTrie opens the main account trie.
func (db *cachingDB) OpenTrie(root common.Hash) (Trie, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	for i := len(db.pastTries) - 1; i >= 0; i-- {   // 优先 从全局的 SecureTrie 缓存中 获取 被 上一个block 中 被commit 的 StateDB Trie
		if db.pastTries[i].Hash() == root {
			return cachedTrie{db.pastTries[i].Copy(), db}, nil // 封装成 cachedTrie
		}
	}
	tr, err := trie.NewSecure(root, db.db, MaxTrieCacheGen)  // cachelimit = 120
	if err != nil {
		return nil, err
	}
	return cachedTrie{tr, db}, nil
}

func (db *cachingDB) pushTrie(t *trie.SecureTrie) { // 将 某个 SecureTrie 放到全局的 cachingDB 的 SecureTrie 缓存数组中.  <其实 能调到这里的 SecureTrie 都是 StateDB Trie 而不是 StateObject Trie>
	db.mu.Lock()
	defer db.mu.Unlock()

	if len(db.pastTries) >= maxPastTries {
		copy(db.pastTries, db.pastTries[1:])
		db.pastTries[len(db.pastTries)-1] = t
	} else {
		db.pastTries = append(db.pastTries, t)
	}
}

// OpenStorageTrie opens the storage trie of an account.
func (db *cachingDB) OpenStorageTrie(addrHash, root common.Hash) (Trie, error) {  // 打开 StateObject Trie
	return trie.NewSecure(root, db.db, 0)
}

// CopyTrie returns an independent copy of the given trie.
func (db *cachingDB) CopyTrie(t Trie) Trie {
	switch t := t.(type) {
	case cachedTrie:
		return cachedTrie{t.SecureTrie.Copy(), db}
	case *trie.SecureTrie:
		return t.Copy()
	default:
		panic(fmt.Errorf("unknown trie type %T", t))
	}
}

// ContractCode retrieves a particular contract's code.
func (db *cachingDB) ContractCode(addrHash, codeHash common.Hash) ([]byte, error) {
	code, err := db.db.Node(codeHash)
	if err == nil {
		db.codeSizeCache.Add(codeHash, len(code))
	}
	return code, err
}

// ContractCodeSize retrieves a particular contracts code's size.
func (db *cachingDB) ContractCodeSize(addrHash, codeHash common.Hash) (int, error) {
	if cached, ok := db.codeSizeCache.Get(codeHash); ok {
		return cached.(int), nil
	}
	code, err := db.ContractCode(addrHash, codeHash)
	return len(code), err
}

// TrieDB retrieves any intermediate trie-node caching layer.
func (db *cachingDB) TrieDB() *trie.Database {
	return db.db
}

// cachedTrie inserts its trie into a cachingDB on commit.
//
// cacheTrie 是 SecureTrie的封装,
//			我们可以知道 其实最终底层操作的都是 SecureTrie
type cachedTrie struct {
	*trie.SecureTrie	// cachingTire 最终使用 SecureTrie  (cachedTrie继承了SecureTrie)
	db *cachingDB  		// cachingDB 中 也有 SecureTrie 数组  和 LRU 缓存(存放codeHash和code的)
}

// StateDB 调用
func (m cachedTrie) Commit(onleaf trie.LeafCallback) (common.Hash, error) {
	root, err := m.SecureTrie.Commit(onleaf)  // 但是从这里我们知道其实 最终 StateDB 也是一颗SecureTrie的哦
	if err == nil {
		m.db.pushTrie(m.SecureTrie)
	}
	return root, err
}


// cachedTrie,其实用的是 SecureTrie 的 Prove
func (m cachedTrie) Prove(key []byte, fromLevel uint, proofDb ethdb.Putter) error {
	return m.SecureTrie.Prove(key, fromLevel, proofDb)
}
