package node

import (
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func historyAPI(t *testing.T) *NodeApi {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	st := storage.NewBadgerStorage(db)
	t.Cleanup(st.Close)
	api := &NodeApi{node: &node.Node{Storage: st}}
	api.vpn.server = exit_gateway.NewSOCKS5Server("127.0.0.1:0", nil)
	return api
}

var exitA, exitB, exitC = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)

// Each exit used becomes one session: switching closes one and opens the next, disconnecting
// closes the last, and the list comes newest first with how long each exit took to serve us.
func TestVPNHistoryRecordsSessions(t *testing.T) {
	api := historyAPI(t)
	assert.Empty(t, api.GetVPNHistory())
	assert.Error(t, api.ConnectLastExit(), "nothing to reconnect to yet")

	api.vpn.mu.Lock()
	api.beginSessionLocked(exitA)
	api.vpn.exitState = ExitOK
	api.noteSessionStateLocked(ExitOK, &exit_gateway.AccountInfo{PricePerMB: 49})
	api.vpn.mu.Unlock()

	list := api.GetVPNHistory()
	require.Len(t, list, 1)
	assert.True(t, list[0].Active)
	assert.Equal(t, exitA, list[0].PeerID)
	assert.Positive(t, list[0].ReadyMs, "the time until the exit served us is kept")
	assert.Equal(t, ExitOK, list[0].LastState)

	api.vpn.mu.Lock()
	api.endSessionLocked("switched")
	api.beginSessionLocked(exitB)
	api.endSessionLocked("stopped")
	api.vpn.mu.Unlock()

	list = api.GetVPNHistory()
	require.Len(t, list, 2)
	assert.Equal(t, exitB, list[0].PeerID, "newest first")
	assert.Equal(t, "stopped", list[0].EndReason)
	assert.Equal(t, exitA, list[1].PeerID)
	assert.Equal(t, "switched", list[1].EndReason)
	assert.False(t, list[0].Active || list[1].Active)
	assert.NotZero(t, list[1].EndedAt)
}

// A session the app was closed in the middle of shows as ended where it was last saved.
func TestVPNHistorySurvivesACrash(t *testing.T) {
	api := historyAPI(t)
	api.vpn.mu.Lock()
	api.beginSessionLocked(exitC) // saved at once
	api.vpn.session = nil         // the app dies without disconnecting
	api.vpn.mu.Unlock()

	list := api.GetVPNHistory()
	require.Len(t, list, 1)
	assert.Equal(t, "app_closed", list[0].EndReason)
	assert.Equal(t, list[0].UpdatedAt, list[0].EndedAt)
}

// Clearing keeps only the session in use; the stored list never grows past its cap.
func TestVPNHistoryClearAndCap(t *testing.T) {
	api := historyAPI(t)
	var many []VPNSession
	for i := 0; i < maxVPNHistory+20; i++ {
		many = append(many, VPNSession{PeerID: exitA, StartedAt: int64(i + 1)})
	}
	require.NoError(t, saveVPNHistory(api.historyStorage(), many))
	assert.Len(t, api.GetVPNHistory(), maxVPNHistory)

	api.vpn.mu.Lock()
	api.beginSessionLocked(exitB)
	api.vpn.mu.Unlock()
	require.NoError(t, api.ClearVPNHistory())
	list := api.GetVPNHistory()
	require.Len(t, list, 1)
	assert.Equal(t, exitB, list[0].PeerID)
}

// The history adds up per exit: one entry per exit however many times it was used, with the
// user's name and star, starred exits first, and the price the exit itself asked.
func TestSavedExitsGroupNameAndStar(t *testing.T) {
	api := historyAPI(t)
	api.vpn.mu.Lock()
	api.beginSessionLocked(exitA)
	api.noteSessionStateLocked(ExitOK, &exit_gateway.AccountInfo{PricePerMB: 49})
	api.endSessionLocked("stopped")
	api.beginSessionLocked(exitB)
	api.endSessionLocked("stopped")
	api.beginSessionLocked(exitA) // in use now
	api.vpn.mu.Unlock()

	exits := api.GetSavedExits()
	require.Len(t, exits, 2, "one entry per exit")
	assert.Equal(t, exitA, exits[0].PeerID, "the most recently used first")
	assert.Equal(t, 2, exits[0].Sessions)
	assert.True(t, exits[0].Active)
	assert.True(t, exits[0].PriceKnown, "an older session knew the price")
	assert.Equal(t, int64(49), exits[0].PricePerMB)
	assert.False(t, exits[1].PriceKnown, "never told: shown as unknown, not free")

	require.NoError(t, api.SetExitName(exitB, "  Friend in Warsaw  "))
	require.NoError(t, api.SetExitFavorite(exitB, true))
	require.NoError(t, api.SetExitFavorite(exitC, true)) // starred before ever used
	exits = api.GetSavedExits()
	require.Len(t, exits, 3)
	assert.Equal(t, exitB, exits[0].PeerID, "favourites first, most recently used first among them")
	assert.Equal(t, "Friend in Warsaw", exits[0].Name)
	assert.Equal(t, exitC, exits[1].PeerID)
	assert.Zero(t, exits[1].LastUsed)
	assert.Error(t, api.SetExitName("not-an-id", "x"))

	require.NoError(t, api.ForgetExit(exitB))
	for _, e := range api.GetSavedExits() {
		assert.NotEqual(t, exitB, e.PeerID, "forgotten with its sessions and name")
	}
}

// What is left on an exit stays with it after we leave: its entry shows the balance the exit
// last reported, from its newest session that heard one, and a session that never heard keeps 0.
func TestSavedExitsKeepTheRemainingBalance(t *testing.T) {
	api := historyAPI(t)
	api.vpn.mu.Lock()
	api.beginSessionLocked(exitA)
	api.noteSessionStateLocked(ExitOK, &exit_gateway.AccountInfo{Enabled: true, PricePerMB: 49, FreeLeft: 1 << 30,
		Credit: 2 << 30, Vault: &exit_gateway.VaultTerms{Credit: 1 << 30}})
	api.endSessionLocked("switched")
	api.beginSessionLocked(exitB)
	api.noteSessionStateLocked(ExitOK, &exit_gateway.AccountInfo{PricePerMB: 0}) // free exit, no account
	api.endSessionLocked("switched")
	api.beginSessionLocked(exitA) // back on A: the exit has not answered yet
	api.vpn.mu.Unlock()

	byID := map[string]SavedExit{}
	for _, e := range api.GetSavedExits() {
		byID[e.PeerID] = e
	}
	a := byID[exitA]
	assert.Equal(t, int64(4<<30), a.Remaining, "free, paid and voucher traffic left on the exit")
	assert.Positive(t, a.RemainingAt)
	assert.Zero(t, byID[exitB].RemainingAt, "an exit without payments reports no balance")

	api.vpn.mu.Lock()
	api.noteSessionStateLocked(ExitOK, &exit_gateway.AccountInfo{Enabled: true, Credit: 1 << 30})
	api.vpn.mu.Unlock()
	assert.Equal(t, int64(1<<30), api.GetSavedExits()[0].Remaining, "the newest report wins")
}
