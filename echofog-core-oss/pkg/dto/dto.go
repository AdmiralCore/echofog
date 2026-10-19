package dto

import (
	"bytes"
	"encoding/gob"
)

type MessageType uint32

const (
	MsgTypeText MessageType = iota
	MsgTypePhoto
	MsgTypeVoice
	MsgTypeCircle
	MsgTypeFile
	MsgTypeLocation
)

type MessageDTO struct {
	ID           string `json:"id"`
	IsMine       bool   `json:"is_mine"`
	SenderID     string `json:"sender_id"`
	TargetID     string `json:"target_id"`
	TargetPubKey string `json:"target_pub_key,omitempty"`

	Type MessageType `json:"type"`

	Text        string `json:"text,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Metadata    string `json:"metadata,omitempty"`

	Timestamp int64 `json:"timestamp"`

	ReplyToID string `json:"reply_to_id,omitempty"`
	AppID     string `json:"app_id,omitempty"`

	// Pending: our own message that has not reached the recipient yet and waits in the outbox.
	Pending bool `json:"pending,omitempty"`
}

type PeerDTO struct {
	PeerID          string `json:"peer_id"`
	ShortID         string `json:"short_id"`
	Address         string `json:"addr,omitempty"`
	ProtocolVersion uint32 `json:"protocol_version"`
	IsConnected     bool   `json:"is_connected"`
	IsLocal         bool   `json:"is_local"`
	LastSeen        string `json:"last_seen"`
}

type FileDTO struct {
	Name string `json:"name,omitempty"`
	Size uint64 `json:"size,omitempty"`
	Path string `json:"path,omitempty"`
}

type LogDTO struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type FileMetadata struct {
}

type SharingPolicyDTO struct {
	Mode            string           `json:"mode"`
	FreeForFriends  bool             `json:"free_for_friends"`
	PricePerMB      int64            `json:"price_per_mb"`
	WhitelistedCIDs []string         `json:"whitelisted_cids"`
	BlacklistedCIDs []string         `json:"blacklisted_cids"`
	PaidCIDs        map[string]int64 `json:"paid_cids,omitempty"`
}

type BackgroundTaskDTO struct {
	Name         string `json:"name"`
	IntervalSec  int64  `json:"interval_sec"`
	Status       string `json:"status"`
	LastRun      string `json:"last_run"`
	LastDuration string `json:"last_duration"`
	RunsCount    uint64 `json:"runs_count"`
	ErrorsCount  uint64 `json:"errors_count"`
	LastError    string `json:"last_error,omitempty"`
}

type ContactDTO struct {
	PeerID   string `json:"peer_id"`
	PubKey   string `json:"pub_key"`
	Alias    string `json:"alias"`
	Avatar   string `json:"avatar_base64"`
	IsOnline bool   `json:"is_online"`
	LastSeen int64  `json:"last_seen"`
}

func (c *ContactDTO) ToBytes() ([]byte, error) {
	var buf bytes.Buffer

	enc := gob.NewEncoder(&buf)

	err := enc.Encode(c)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func ContactFromBytes(data []byte) (ContactDTO, error) {
	buf := bytes.NewReader(data)
	dec := gob.NewDecoder(buf)

	var contact ContactDTO
	err := dec.Decode(&contact)
	return contact, err
}

type GridConfigDTO struct {
	EnableRelay         bool   `json:"enable_relay"`
	MaxBandwidthMB      int    `json:"max_bandwidth_mb"`
	DailyLimitGB        int    `json:"daily_limit_gb"`
	SmartThrottle       bool   `json:"smart_throttle"`
	EnableStorage       bool   `json:"enable_storage"`
	StorageAllocationGB int    `json:"storage_allocation_gb"`
	StoragePath         string `json:"storage_path"`
	EnableAI            bool   `json:"enable_ai"`
	SelectedAIModel     string `json:"selected_ai_model"`
	AllowMCPTools       bool   `json:"allow_mcp_tools"`
	EnableWasmCompute   bool   `json:"enable_wasm_compute"`
	CPUCores            int    `json:"cpu_cores"`
	IsProviderLive      bool   `json:"is_provider_live"`
	AIEndpoint          string `json:"ai_endpoint"`
	AllowRemoteLLM      bool   `json:"allow_remote_llm"`
}

type HardwareInfoDTO struct {
	GPUModel     string `json:"gpu_model"`
	VRAMTotalMB  int    `json:"vram_total_mb"`
	CPUCores     int    `json:"cpu_cores"`
	RAMTotalMB   int    `json:"ram_total_mb"`
	DiskFreeGB   int    `json:"disk_free_gb"`
	OS           string `json:"os"`
	PlatformArch string `json:"platform_arch"`
}

type GridAttestationResultDTO struct {
	Success        bool     `json:"success"`
	LatencyScoreMs int      `json:"latency_score_ms"`
	MerkleValid    bool     `json:"merkle_valid"`
	ReasoningValid bool     `json:"reasoning_valid"`
	AttesterPeers  []string `json:"attester_peers"`
	NodeScore      float64  `json:"node_score"`
	RewardRateFOG  float64  `json:"reward_rate_fog"`
	Failures       []string `json:"failures"`
}

// NodeEventDTO represents an event transported to the UI and external AI clients
type NodeEventDTO struct {
	Topic     string `json:"topic"`
	Timestamp int64  `json:"timestamp"`
	Payload   any    `json:"payload"`
}

// MCPToolDTO represents a tool metadata definition for UI and AI callers
type MCPToolDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
	InputSchema any    `json:"input_schema,omitempty"`
}

// MCPStatusDTO represents current operational status of the MCP server
type MCPStatusDTO struct {
	AccessLevel      string `json:"access_level"`
	AllowRemote      bool   `json:"allow_remote"`
	IsRunning        bool   `json:"is_running"`
	ToolsCount       int    `json:"tools_count"`
	SubscribersCount int    `json:"subscribers_count"`
	EventBusAttached bool   `json:"event_bus_attached"`
}

// StreamQualityDTO is a selectable broadcast quality preset (up to 4K60)
type StreamQualityDTO struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	BitrateKbps int    `json:"bitrateKbps"`
	Codec       string `json:"codec"`
	Fits        bool   `json:"fits"` // true if the local upload capacity can sustain this preset
}

// StreamInfoDTO describes an announced stream together with its quality
type StreamInfoDTO struct {
	StreamID    string `json:"streamId"`
	Broadcaster string `json:"broadcaster"`
	MediaType   string `json:"mediaType"`
	Bitrate     int    `json:"bitrate"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	Codec       string `json:"codec"`
	QualityID   string `json:"qualityId"`
}

// CallMediaSlotDTO tells the UI which remote speaker or stream plays on a loopback track.
// The track's MediaStream id is "efslot-" + Slot.
type CallMediaSlotDTO struct {
	Slot     string `json:"slot"`
	Kind     string `json:"kind"` // "audio" | "video"
	Peer     string `json:"peer"`
	StreamID string `json:"streamId"`
	Active   bool   `json:"active"`
}

// CallRosterDTO lists the members of the active call.
type CallRosterDTO struct {
	CallID      string   `json:"callId"`
	Self        string   `json:"self"`
	Coordinator string   `json:"coordinator"`
	Members     []string `json:"members"`
	Streams     []string `json:"streams"`
}

// TonWalletDTO describes the built-in TON wallet.
type TonWalletDTO struct {
	Address    string `json:"address"`    // friendly, non-bounceable (UQ… / 0Q…)
	RawAddress string `json:"rawAddress"` // 0:hex
	Network    string `json:"network"`    // "mainnet" | "testnet"
	TopUpLink  string `json:"topUpLink"`  // ton://transfer/… for the receive QR code
	Frozen     bool   `json:"frozen"`
}

// VaultDTO is the user's EchoFogVault: the TON deposit paid proxy nodes are paid from.
type VaultDTO struct {
	Address     string `json:"address"`     // the vault contract (EQ… / kQ…)
	Deployed    bool   `json:"deployed"`    // false until the first deposit
	DepositTON  string `json:"depositTon"`  // what nodes can still be paid from
	Closing     bool   `json:"closing"`     // a withdrawal is pending; nodes no longer sell against it
	WithdrawAt  int64  `json:"withdrawAt"`  // unix seconds when the withdrawal can be completed (0: none)
	CanFinish   bool   `json:"canFinish"`   // the 24 hours are over: the deposit can be taken back
	MinFirstTON string `json:"minFirstTon"` // smallest first deposit (it also creates the vault)
}

// VoucherEarningsDTO is what this node's exit earned from vouchers.
type VoucherEarningsDTO struct {
	OwedTON   string `json:"owedTon"`   // signed to us, not cashed yet
	CashedTON string `json:"cashedTon"` // already paid into our wallet
	QuoteNano int64  `json:"quoteNano"` // current price in nanoton per MB (0: not taking vouchers)
	// ClaimProblem: why what is owed is not being cashed ("" when fine), e.g. an empty wallet
	ClaimProblem string `json:"claimProblem,omitempty"`
	WalletAddr   string `json:"walletAddr,omitempty"` // where to send TON for the fee
}

// SupportDTO is the state of the voluntary project share of traffic payments.
type SupportDTO struct {
	Available   bool   `json:"available"`   // false: no project address configured, show nothing
	Percent     int    `json:"percent"`     // 0 = off
	PendingTON  string `json:"pendingTon"`  // set aside, not yet sent
	PendingUSDT string `json:"pendingUsdt"` // set aside, not yet sent
	Address     string `json:"address"`
}

// TonBalanceDTO is the wallet balance; Nano is a decimal string to keep full precision in JS.
type TonBalanceDTO struct {
	Nano     string  `json:"nano"`
	TON      string  `json:"ton"`
	Deployed bool    `json:"deployed"`
	USDRate  float64 `json:"usdRate"` // 0 when the price is unavailable
	// USDT on TON (jetton). USDTAvailable is false on networks without USDT or when the
	// token balance could not be read; USDT is a decimal string with 6 decimals.
	USDTAvailable bool   `json:"usdtAvailable"`
	USDT          string `json:"usdt"`
}

// TonTxDTO is one wallet history entry.
type TonTxDTO struct {
	Asset        string `json:"asset"` // "TON" | "USDT"
	Hash         string `json:"hash"`
	Time         int64  `json:"time"` // unix seconds
	Incoming     bool   `json:"incoming"`
	TON          string `json:"ton"` // amount in Asset units (kept as "ton" for compatibility)
	Counterparty string `json:"counterparty"`
	Comment      string `json:"comment"`
	FeeTON       string `json:"feeTon"`
	Failed       bool   `json:"failed"`
}

// FileTransferDTO is a file sent to or received from a contact.
type FileTransferDTO struct {
	ID      string `json:"id"`
	Peer    string `json:"peer"`
	Dir     string `json:"dir"` // "out": we send it; "in": we receive it
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mime    string `json:"mime,omitempty"`
	State   string `json:"state"` // offered, active, done, declined, cancelled, failed
	Error   string `json:"error,omitempty"`
	Path    string `json:"path,omitempty"`  // the file on this computer (the source, or the received file once done)
	Done    int64  `json:"done"`            // bytes received and checked, or sent
	Rate    int64  `json:"rate"`            // bytes per second, lately
	Stalled bool   `json:"stalled"`         // nothing moves: the peer is offline or cannot be reached
	Group   string `json:"group,omitempty"` // the group chat the file was posted in
	// a file we posted in a group: members it was offered to, who got all of it, who fetch it now
	Members   int   `json:"members,omitempty"`
	Delivered int   `json:"delivered,omitempty"`
	Fetching  int   `json:"fetching,omitempty"`
	Created   int64 `json:"created"` // unix milliseconds
	Updated   int64 `json:"updated"`
}

// SavedMessageDTO is a message the user bookmarked: a copy, so it stays even when the chat goes.
type SavedMessageDTO struct {
	ID       string `json:"id"`     // chat ID and message ID: one bookmark per message
	ChatID   string `json:"chatId"` // "d:<peer>" or "g:<group>", as the UI names chats
	ChatName string `json:"chatName"`
	MsgID    string `json:"msgId"`
	Sender   string `json:"sender,omitempty"`
	Mine     bool   `json:"mine"`
	Text     string `json:"text"`
	FileName string `json:"fileName,omitempty"`
	FileSize int64  `json:"fileSize,omitempty"`
	Ts       int64  `json:"ts"`      // when the message was written, unix milliseconds
	SavedAt  int64  `json:"savedAt"` // unix milliseconds
}
