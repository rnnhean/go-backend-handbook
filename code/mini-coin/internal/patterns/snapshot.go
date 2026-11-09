package patterns

import "sync/atomic"

// Snapshot 是一份不可变配置快照。
//
// 约定：Store 之后绝不能再修改 Entries。刷新方每次构造一份完整的新快照，
// 然后整体替换指针；读方拿到的永远是一份自洽的数据。
type Snapshot struct {
	Version int64
	Entries map[string]string
}

// SnapshotStore 用 atomic.Value 发布不可变快照，读路径无锁。
type SnapshotStore struct {
	value atomic.Value // *Snapshot
}

// NewSnapshotStore 创建快照存储，可以带一份初始快照。
func NewSnapshotStore(initial *Snapshot) *SnapshotStore {
	s := &SnapshotStore{}
	if initial != nil {
		s.value.Store(initial)
	}
	return s
}

// Load 返回当前快照。没有快照时返回 nil。
func (s *SnapshotStore) Load() *Snapshot {
	v, ok := s.value.Load().(*Snapshot)
	if !ok {
		return nil
	}
	return v
}

// Store 原子发布一份新快照。调用方必须先构造完整快照，发布后不得再修改。
func (s *SnapshotStore) Store(next *Snapshot) {
	s.value.Store(next)
}
