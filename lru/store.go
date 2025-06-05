package lru

import "time"

type Store interface {
	AddAndUpdateCache(key string, value Value) error
	DeleteCache(key string) error
	FindCache(key string) (*Value, bool)
	Close()
}

// 需要传递的初始化参数
type Options struct {
	maxBytes        int64
	onEvicted       func(key string, value Value)
	cleanupInterval time.Duration
}

// CacheType 缓存类型
type CacheType string

const (
	LRU CacheType = "lru"
)

// 工厂模式
func NewStore(cacheType CacheType, opt *Options) Store {
	switch cacheType {
	case LRU:
		return NewLruCache(opt)
	default:
		return NewLruCache(opt)
	}
}
