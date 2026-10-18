package node

import (
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Saved messages are kept, listed newest first, saved once per message, and removed on request.
func TestSavedMessages(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	st := storage.NewBadgerStorage(db)
	t.Cleanup(st.Close)
	api := &NodeApi{node: &node.Node{Storage: st}}

	list, err := api.ListSavedMessages()
	require.NoError(t, err)
	assert.Empty(t, list)

	_, err = api.SaveMessage(dto.SavedMessageDTO{Text: "no chat"})
	assert.Error(t, err, "a bookmark needs its chat and message")

	a, err := api.SaveMessage(dto.SavedMessageDTO{ChatID: "d:aa", ChatName: "Alice", MsgID: "m1", Text: "first", Ts: 1})
	require.NoError(t, err)
	assert.Equal(t, "d:aa/m1", a.ID)
	_, err = api.SaveMessage(dto.SavedMessageDTO{ChatID: "g:grp", ChatName: "Team", MsgID: "m2", Text: strings.Repeat("я", 40000), FileName: "x.pdf", FileSize: 9})
	require.NoError(t, err)
	// saving the same message again replaces the copy
	_, err = api.SaveMessage(dto.SavedMessageDTO{ChatID: "d:aa", ChatName: "Alice", MsgID: "m1", Text: "first, edited"})
	require.NoError(t, err)

	list, err = api.ListSavedMessages()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "d:aa/m1", list[0].ID, "the latest saved comes first")
	assert.Equal(t, "first, edited", list[0].Text)
	assert.LessOrEqual(t, len(list[1].Text), maxSavedText, "very long text is cut")
	assert.True(t, strings.HasPrefix(list[1].Text, "я"))
	assert.Equal(t, "x.pdf", list[1].FileName)

	require.NoError(t, api.UnsaveMessage("d:aa/m1"))
	list, err = api.ListSavedMessages()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "g:grp/m2", list[0].ID)
}

func TestFileKeyNeverReachesTheUI(t *testing.T) {
	m := scrubMessage(dto.MessageDTO{Type: dto.MsgTypeFile, Metadata: `{"file_name":"a.txt","file_size":3,"file_key_hex":"00ff","custom":{"fid":"ab","root":"cd"}}`})
	assert.NotContains(t, m.Metadata, "file_key_hex")
	assert.NotContains(t, m.Metadata, "00ff")
	assert.Contains(t, m.Metadata, `"fid":"ab"`)
	text := dto.MessageDTO{Type: dto.MsgTypeText, Text: "hi", Metadata: `{"app_id":"chat"}`}
	assert.Equal(t, text, scrubMessage(text))
}
