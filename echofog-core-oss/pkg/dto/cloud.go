package dto

type CloudFileDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      uint64 `json:"size"`
	RootHash  string `json:"root_hash"`
	CreatedAt int64  `json:"created_at"`
}
