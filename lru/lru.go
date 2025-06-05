package lru

import (
	"container/list"
	"fmt"
	"go.uber.org/zap"
	"sync"
	"time"
)

// lurCache 是一个简单的 LRU 缓存实现，底层基于一个链表和一个 map实现。
// 它支持添加、获取和删除缓存项，并在缓存超过最大容量时自动删除最旧的项，并且会在超时时间之后删除相应的内容。
// 缓存项的过期时间可以通过设置超时时间来设置。
// 该实现是线程安全的，使用了互斥锁来保护对缓存的并发访问。
// 该实现还支持自定义的过期回调函数，当缓存项被删除时会调用该函数。

// 创建lru cache结构体，我将从核心到辅助功能进行分层设计,并且将外层容器结构体和内层条路结构体分离的设计策略
// 外层容器结构体
type LruCache struct {
	// 1.首先是核心功能：数据存储、容量控制、并发控制
	list         *list.List               // 双向链表，用于维护lru顺序
	items        map[string]*list.Element // 键到链表节点的映射
	maxBytes     int64                    // 最大容量
	currentBytes int64                    // 当前已经使用的容量
	mu           sync.RWMutex             // 读写锁
	// 2.其次是扩展功能：淘汰策略、过期机制
	onEvicted func(key string, value Value) // 作为扩展点，初期可以设置为nil，后续按需实现
	expires   map[string]time.Time          // 为每个键值对存储过期时间，支持自动清理（TTL）
	// 3.最后是优化功能：后台清理协程、优雅关闭、监控统计（命中率、吞吐量）
	cleanupInterval time.Duration // 后台自动清理过期键值对 的时间间隔参数
	cleanTicker     *time.Ticker  // 自动清理过期键值对的定时
	closeChan       chan struct{} // 用于优雅关闭清理协程
	// 日志输出
	log *zap.Logger
}

// 内层条目结构体
type LruEntry struct {
	key   string
	value Value
}

type Value interface {
	Len() int
}

// 构造函数
func NewLruCache(opt *Options) *LruCache {
	withDefault(opt)
	cache := &LruCache{
		list:            list.New(),
		items:           make(map[string]*list.Element),
		maxBytes:        opt.maxBytes,
		currentBytes:    0,
		onEvicted:       opt.onEvicted,
		expires:         make(map[string]time.Time),
		cleanupInterval: opt.cleanupInterval,
		closeChan:       make(chan struct{}),
	}
	cache.startCleanUpRoutine()
	return cache
}
func withDefault(opt *Options) {
	if opt.cleanupInterval <= 0 {
		opt.cleanupInterval = time.Minute
	}
	if opt.maxBytes <= 0 {
		opt.maxBytes = 8 * 1024 * 1024
	}
}

func (c *LruCache) startCleanUpRoutine() {
	// 启动定期清理数据协程
	c.cleanTicker = time.NewTicker(c.cleanupInterval)
	go func() {
		err := c.cleanupLoop()
		if err != nil {
			c.log.Error(err.Error())
		}
	}()
}

// 1.向缓存中新增/更新数据
// 分为两种情况：一种是需要更新 一种是需要添加
func (c *LruCache) AddAndUpdateCache(key string, value Value) error {
	if value == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 首先应该先判断key是否在缓存中已经存在了，如果存在了，则更新该key的内容
	if elem, ok := c.items[key]; ok {
		err := c.update(elem, value)
		if err != nil {
			c.log.Error(err.Error())
			return fmt.Errorf("AddAndUpdateCache 更新失败:%v", err.Error())
		}
		return nil
	}

	// 如果不存在话，将新数据添加到缓存中
	c.add(key, value)
	// 更新一下当前的容量
	c.currentBytes += int64(len(key) + value.Len())
	// 重新设置该key对应的失效时间映射关系
	c.createExpires(key)
	// 清理一下超时的缓存数据和处理一下存储空间不足的问题
	err := c.evict()
	if err != nil {
		c.log.Error(err.Error())
		return fmt.Errorf("AddAndUpdateCache 删除超过容量或者过期的数据报错:%v", err.Error())
	}
	return nil
}
func (c *LruCache) add(key string, value Value) {
	// 首先我需要将该元素插入到list的尾部
	entry := &LruEntry{
		key:   key,
		value: value,
	}
	backElem := c.list.PushBack(&entry)
	// 然后获取这个元素插入到map映射中
	c.items[key] = backElem
}

// 更新key对应的值，并且将该元素放到list的尾部
func (c *LruCache) update(elem *list.Element, value Value) error {
	entry := elem.Value.(*LruEntry)
	// 首先需要判断一下更新后的容量大小是否已经超过了最大容量
	cbytes := c.currentBytes + int64(value.Len()-entry.value.Len())
	if cbytes > c.maxBytes {
		return fmt.Errorf("update 更新过后的存储大小超过最大容量，无法更新")
	}
	c.currentBytes += int64(value.Len() - entry.value.Len())
	entry.value = value
	c.list.MoveToBack(elem)
	return nil
}

// 创建元素的超时时间  什么时候超时
func (c *LruCache) createExpires(key string) {
	resultExp := time.Now().Add(c.cleanupInterval)
	c.expires[key] = resultExp
}

// 2.根据key删除缓存中的数据
func (c *LruCache) DeleteCache(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		err := c.removeCache(element)
		if err != nil {
			c.log.Error("DeleteCache 删除节点报错")
			return fmt.Errorf("DeleteCache 删除节点报错:%v", err.Error())
		}
	}
	return nil
}

// 4.查询缓存中的数据
func (c *LruCache) FindCache(key string) (*Value, bool) {
	// 首先应该先确认key是否存在并且判断key是否超时了，如果存在且没有超时则取出来，并且将该元素放到列表尾部，如果不存在或者超时了，则查询数据库
	c.mu.RLock()
	element, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	// 判断该元素是否超时
	// 获取超时时间与当前时间作比较
	if t, ok := c.expires[key]; ok && time.Now().After(t) {
		c.mu.RUnlock()
		// 直接删除这个key并且返回
		go func() {
			err := c.DeleteCache(key)
			if err != nil {
				return
			}
		}()
	}
	value := element.Value.(LruEntry).value
	c.mu.RUnlock()
	// 将当前访问到的元素移动到list的队尾，移动时，需要设置写锁
	c.mu.Lock()
	// 再次检查元素是否仍然存在（可能在获取写锁期间被其他协程删除）
	if _, ok := c.items[key]; ok {
		c.list.MoveToBack(element)
	}
	c.mu.Unlock()
	return &value, true
}
func (c *LruCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.list.Len()
}

// 5.删除缓存中的数据
func (c *LruCache) removeCache(elem *list.Element) error {
	// 1.从缓存中删除传进来的元素
	// 1.1.首先删除list中的数据
	entry := elem.Value.(*LruEntry)
	c.list.Remove(elem)
	// 1.2.再删除掉map中的映射关系
	delete(c.items, entry.key)
	delete(c.items, entry.key)
	// 2.修改缓存的当前存储空间
	c.currentBytes -= int64(len(entry.key) + entry.value.Len())
	if c.onEvicted != nil {
		c.onEvicted(entry.key, entry.value)
	}
	return nil
}

// 定期清理缓存的方法
func (c *LruCache) cleanupLoop() error {
	for {
		select {
		// 如果检测到时间到了，那么就执行清楚缓存中已经超过过期时间的数据，从而实现定期清理过期数据
		case <-c.cleanTicker.C:
			c.mu.Lock()
			err := c.evict()
			if err != nil {
				c.log.Error(err.Error())
				return fmt.Errorf("cleanupLoop 报错:%v", err.Error())
			}
			c.mu.Unlock()
		case <-c.closeChan:
			return nil

		}
	}
}

// evict 清理过期和超出内存限制的缓存，调用此方法前必须持有锁
func (c *LruCache) evict() error {
	// 首先先处理过期数据
	now := time.Now()
	expires := c.expires
	for i, j := range expires {
		// 判断是否超时了，如果超时的话，就执行去除函数
		if now.After(j) {
			if elem, ok := c.items[i]; ok {
				err := c.removeCache(elem)
				if err != nil {
					c.log.Error(err.Error())
					return fmt.Errorf("evict 清理过期数据报错:%v", err.Error())
				}
			}
		}

	}
	// 当存储的数据大小超出了最大存储的时候，需要根据lru策略删除掉缓存中的数据
	// 如果超出了最大存储，那么应该从list的头部开始删除数据，直到删到当前存储小于最大存储的时候
	for c.currentBytes > c.maxBytes && c.maxBytes > 0 && c.list.Len() > 0 {
		elem := c.list.Front() // 获取最久未使用的项（链表头部）
		if elem != nil {
			err := c.removeCache(elem)
			if err != nil {
				c.log.Error(err.Error())
				return fmt.Errorf("evict 清理超过最大缓存的数据报错:%v", err.Error())
			}
		}
	}
	return nil
}

// close 关闭缓存，停止清理协程
func (c *LruCache) Close() {
	if c.cleanTicker != nil {
		c.cleanTicker.Stop()
		close(c.closeChan)
	}
}
