package settings

import (
	"errors"
	"testing"
)

func TestNormalizeCanonicalizesAllowlists(t *testing.T) {
	value, err := Normalize(Settings{
		AppID: "campus-services", Enabled: true, WSURL: "ws://127.0.0.1:3001", BotQQID: "2647414417",
		AllowedGroupIDs: []string{"200", "100", "200"}, AllowedPrivateUserIDs: []string{"300"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(value.AllowedGroupIDs) != 2 || value.AllowedGroupIDs[0] != "100" || value.AllowedGroupIDs[1] != "200" {
		t.Fatalf("群白名单未规范化：%#v", value.AllowedGroupIDs)
	}
}

func TestNormalizeRejectsInvalidSettings(t *testing.T) {
	tests := []Settings{
		{AppID: "campus-services", Enabled: true, BotQQID: "2647414417"},
		{AppID: "campus-services", Enabled: true, WSURL: "http://127.0.0.1:3001", BotQQID: "2647414417"},
		{AppID: "campus-services", Enabled: true, WSURL: "ws://127.0.0.1:3001?access_token=secret", BotQQID: "2647414417"},
		{AppID: "campus-services", Enabled: true, WSURL: "ws://127.0.0.1:3001", BotQQID: "0"},
		{AppID: "campus-services", AllowedGroupIDs: []string{"00123"}},
		{AppID: "../app"},
	}
	for index, test := range tests {
		if _, err := Normalize(test); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d error=%v", index, err)
		}
	}
}

func TestDisabledSettingsMayStartEmpty(t *testing.T) {
	value, err := Normalize(Settings{AppID: "campus-services"})
	if err != nil || value.Enabled {
		t.Fatalf("value=%#v err=%v", value, err)
	}
}
