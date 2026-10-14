package messenger

import "testing"

func FuzzDecodeMetadata(f *testing.F) {
	f.Add(`{"app_id":"chat"}`)
	f.Add(``)
	f.Add(`[1,2,3]`)
	f.Add(`{"numeric_app_id":-1}`)
	f.Fuzz(func(t *testing.T, raw string) {
		_ = DecodeMetadata(raw)
		_ = EncodeMetadata(&MessageMetadata{AppID: raw})
	})
}
