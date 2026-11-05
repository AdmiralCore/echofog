package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v1NetBackend is the protocol 1 implementation of netBackend: the code that used to sit in the
// NodeApi methods of this domain, moved here unchanged by T11.1b. The NodeApi methods below are
// one-line delegates, so at protocol 1 the bodies run exactly as before, and at protocol 2 the
// same delegates reach v2PendingBackend (see backends.go).
type v1NetBackend struct{ *NodeApi }

// netDomain is the network backend of this facade: the one chosen at start-up, or — for a facade
// built without NewNodeApi, as the package tests do — the choice NewNodeApi would have made.
func (n *NodeApi) netDomain() netBackend {
	if n.backends.net != nil {
		return n.backends.net
	}
	if n.node != nil && n.node.Cfg != nil && n.node.Cfg.ProtocolOrDefault() == config.ProtocolV2 {
		return v2PendingBackend{}
	}
	return &v1NetBackend{n}
}

func (n *NodeApi) AddBootstrapNode(entry string) error {
	return n.netDomain().AddBootstrapNode(entry)
}

func (n *NodeApi) AddRelay(link string) error {
	return n.netDomain().AddRelay(link)
}

func (n *NodeApi) CheckProxy(addr string) error {
	return n.netDomain().CheckProxy(addr)
}

func (n *NodeApi) ConnectToPeer(addr string) error {
	return n.netDomain().ConnectToPeer(addr)
}

func (n *NodeApi) CreatePass(r PassRequest) (PassView, error) {
	return n.netDomain().CreatePass(r)
}

func (n *NodeApi) DeletePass(id string) error {
	return n.netDomain().DeletePass(id)
}

func (n *NodeApi) ExtendPass(id string, r PassRequest) (PassView, error) {
	return n.netDomain().ExtendPass(id, r)
}

func (n *NodeApi) ForgetInvite(peerID string) error {
	return n.netDomain().ForgetInvite(peerID)
}

func (n *NodeApi) GetAccessStatus() (AccessStatus, error) {
	return n.netDomain().GetAccessStatus()
}

func (n *NodeApi) GetAnonymity() (AnonymityStatus, error) {
	return n.netDomain().GetAnonymity()
}

func (n *NodeApi) GetBootstrapNodes() (BootstrapStatus, error) {
	return n.netDomain().GetBootstrapNodes()
}

func (n *NodeApi) GetGuardStatus() (GuardStatus, error) {
	return n.netDomain().GetGuardStatus()
}

func (n *NodeApi) GetNetworkStats() NetworkStats {
	return n.netDomain().GetNetworkStats()
}

func (n *NodeApi) GetPass(id string) (PassView, error) {
	return n.netDomain().GetPass(id)
}

func (n *NodeApi) GetPeers() []dto.PeerDTO {
	return n.netDomain().GetPeers()
}

func (n *NodeApi) GetProxySettings() (ProxyStatus, error) {
	return n.netDomain().GetProxySettings()
}

func (n *NodeApi) GetRelaySettings() (RelaySettingsDTO, error) {
	return n.netDomain().GetRelaySettings()
}

func (n *NodeApi) GetRelayStatus() (RelayStatus, error) {
	return n.netDomain().GetRelayStatus()
}

func (n *NodeApi) JoinInvite(link string) (string, error) {
	return n.netDomain().JoinInvite(link)
}

func (n *NodeApi) ListPasses() ([]PassView, error) {
	return n.netDomain().ListPasses()
}

func (n *NodeApi) MeasureUploadCapacity() (int, error) {
	return n.netDomain().MeasureUploadCapacity()
}

func (n *NodeApi) OnPeerState(cb func(PeerStateDTO)) func() {
	return n.netDomain().OnPeerState(cb)
}

func (n *NodeApi) ParseInvite(link string) (InviteInfo, error) {
	return n.netDomain().ParseInvite(link)
}

func (n *NodeApi) PeerDiscoveryHandler() <-chan dto.PeerDTO {
	return n.netDomain().PeerDiscoveryHandler()
}

func (n *NodeApi) PinGuard(entry string) error {
	return n.netDomain().PinGuard(entry)
}

func (n *NodeApi) RemoveBootstrapNode(entry string) error {
	return n.netDomain().RemoveBootstrapNode(entry)
}

func (n *NodeApi) RemoveJoinedPeer(peerID string) error {
	return n.netDomain().RemoveJoinedPeer(peerID)
}

func (n *NodeApi) RemovePassDevice(id, peerID string) error {
	return n.netDomain().RemovePassDevice(id, peerID)
}

func (n *NodeApi) RevokePass(id string) (PassView, error) {
	return n.netDomain().RevokePass(id)
}

func (n *NodeApi) RotateAccessSecret() (string, error) {
	return n.netDomain().RotateAccessSecret()
}

func (n *NodeApi) SetAccessMode(mode string) error {
	return n.netDomain().SetAccessMode(mode)
}

func (n *NodeApi) SetAnonymity(mode string, walletDirect bool) error {
	return n.netDomain().SetAnonymity(mode, walletDirect)
}

func (n *NodeApi) SetGuardMode(mode string) error {
	return n.netDomain().SetGuardMode(mode)
}

func (n *NodeApi) SetProxySettings(proxy, callRoute string) error {
	return n.netDomain().SetProxySettings(proxy, callRoute)
}

func (n *NodeApi) SetRelaySettings(r RelaySettingsDTO) error {
	return n.netDomain().SetRelaySettings(r)
}

func (n *NodeApi) StartLocalDiscovery(myAlias string) {
	n.netDomain().StartLocalDiscovery(myAlias)
}

func (n *NodeApi) StartVPNVia(target string) error {
	return n.netDomain().StartVPNVia(target)
}

func (n *NodeApi) StopLocalDiscovery() {
	n.netDomain().StopLocalDiscovery()
}

func (n *NodeApi) UnpinGuard(entry string) error {
	return n.netDomain().UnpinGuard(entry)
}
