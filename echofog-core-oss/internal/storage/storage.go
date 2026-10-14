package storage

type Storage interface {
	Set(key, value []byte) error
	Get(key []byte) ([]byte, error)
	Delete(key []byte) error
	FindValues(prefix []byte, limit int) ([]Entry, error)
	FindValuesSeek(prefix []byte, startKey []byte, limit int) ([]Entry, error)
	Exists(key []byte) bool
	Close()
}

type MessageStorage interface {
	Storage
	GetNextID(chatID uint64) (uint64, error)
}
