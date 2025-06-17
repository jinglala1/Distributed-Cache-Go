package main

import (
	"Distributed-Cache-Go/lru"
	"context"
	"go.uber.org/zap"
	"sync"
	"sync/atomic"
	"time"
)

// cache 对于底层的策略进行的封装
type Cache struct {
	// 首先是核心功能 1、底层的存储策略  2、缓存配置项（因为后期需要在默认的缓存配置项上进行延迟初始化，所以直接将配置项放到了属性里面）
	mu           sync.RWMutex
	store        lru.Store
	cacheOptions CacheOptions
	// 状态属性（运行时状态跟踪），用于记录和管理缓存实例的运行状态
	initialized int32 // 原子变量，标记缓存是否已初始化
	closed      int32 // 原子变量，标记缓存是否已关闭
	// 统计属性，用于记录缓存的使用情况
	hits   int64 // 缓存命中次数
	misses int64 // 缓存未命中次数
	log    *zap.Logger
}
type CacheOptions struct {
	CacheType       lru.CacheType
	MaxBytes        int64
	OnEvicted       func(key string, value lru.Value)
	CleanupInterval time.Duration
}

func DefaultCacheOptions() CacheOptions {
	return CacheOptions{
		CacheType:       lru.LRU,
		MaxBytes:        8 * 1024 * 1024, // 8MB
		CleanupInterval: time.Minute,
		OnEvicted:       nil,
	}
}
func NewCache(opt *CacheOptions) *Cache {
	cache := &Cache{
		cacheOptions: *opt,
	}
	return cache
}

// 延迟初始化的函数
func (c *Cache) ensureInitialized() {
	// 首先判断一下当前实例是否已经被初始化了，如果已经被初始化了，那么就直接返回
	if atomic.LoadInt32(&c.initialized) == 1 {
		return
	}
	// 如果当前实例没有被初始化，那么就进行延迟初始化
	Options := &lru.Options{
		CleanupInterval: c.cacheOptions.CleanupInterval,
		MaxBytes:        c.cacheOptions.MaxBytes,
		OnEvicted:       c.cacheOptions.OnEvicted,
	}
	cache := lru.NewStore(c.cacheOptions.CacheType, Options)
	// 将初始化后的缓存实例赋值给当前实例的 store 属性
	c.store = cache
	// 将状态修改为 初始化完成
	atomic.AddInt32(&c.initialized, 1)
	c.log.Info("缓存实例初始化完成")
}

// 增加或者更新
func (c *Cache) Add(key string, value ByteView) {
	// 首先判断一下是否已经进行了初始化
	if atomic.LoadInt32(&c.initialized) == 0 {
		// 执行延迟初始化
		c.ensureInitialized()
	}
	err := c.store.AddAndUpdateCache(key, value)
	if err != nil {
		c.log.Error("缓存增加或者更新失败", zap.Error(err))
	}

}

// 删除
func (c *Cache) Delete(key string) {
	if atomic.LoadInt32(&c.closed) == 1 || atomic.LoadInt32(&c.initialized) == 0 {
		return
	}

	err := c.store.DeleteCache(key)
	if err != nil {
		c.log.Error("缓存删除失败", zap.Error(err))
	}
}

// 查找
func (c *Cache) Get(ctx context.Context, key string) (value ByteView, ok bool) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ByteView{}, false
	}

	// 如果缓存未初始化，直接返回未命中
	if atomic.LoadInt32(&c.initialized) == 0 {
		atomic.AddInt64(&c.misses, 1)
		return ByteView{}, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// 从底层存储获取
	val, found := c.store.FindCache(key)
	if !found {
		atomic.AddInt64(&c.misses, 1)
		return ByteView{}, false
	}

	// 更新命中计数
	atomic.AddInt64(&c.hits, 1)

	// 转换并返回
	if bv, ok := val.(ByteView); ok {
		return bv, true
	}

	// 类型断言失败
	atomic.AddInt64(&c.misses, 1)
	return ByteView{}, false
}
