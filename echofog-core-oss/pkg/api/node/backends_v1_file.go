package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v1FileBackend is the protocol 1 implementation of fileBackend: the code that used to sit in the
// NodeApi methods of this domain, moved here unchanged by T11.1c. The NodeApi methods below are
// one-line delegates.
type v1FileBackend struct{ *NodeApi }

// fileDomain is the files backend of this facade, chosen the same way as netDomain.
func (n *NodeApi) fileDomain() fileBackend {
	if n.backends.file != nil {
		return n.backends.file
	}
	if n.node != nil && n.node.Cfg != nil && n.node.Cfg.ProtocolOrDefault() == config.ProtocolV2 {
		return v2PendingBackend{}
	}
	return &v1FileBackend{n}
}

func (n *NodeApi) AcceptFile(id string) (dto.FileTransferDTO, error) {
	return n.fileDomain().AcceptFile(id)
}

func (n *NodeApi) CancelFile(id string) error {
	return n.fileDomain().CancelFile(id)
}

func (n *NodeApi) DeclineFile(id string) error {
	return n.fileDomain().DeclineFile(id)
}

func (n *NodeApi) DownloadDir() string {
	return n.fileDomain().DownloadDir()
}

func (n *NodeApi) FileTransferPath(id string) (string, error) {
	return n.fileDomain().FileTransferPath(id)
}

func (n *NodeApi) GetFileTransfer(id string) (dto.FileTransferDTO, error) {
	return n.fileDomain().GetFileTransfer(id)
}

func (n *NodeApi) ListFileTransfers(peerIDHex string) ([]dto.FileTransferDTO, error) {
	return n.fileDomain().ListFileTransfers(peerIDHex)
}

func (n *NodeApi) OnFileTransfer(cb func(dto.FileTransferDTO)) func() {
	return n.fileDomain().OnFileTransfer(cb)
}

func (n *NodeApi) SendFile(pubKeyHex string, path string) (dto.FileTransferDTO, error) {
	return n.fileDomain().SendFile(pubKeyHex, path)
}

func (n *NodeApi) SendGroupFile(groupID, channel, thread, path string) (GroupMessageDTO, error) {
	return n.fileDomain().SendGroupFile(groupID, channel, thread, path)
}
