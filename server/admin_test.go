package server

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

func newNonDevTestServer(t *testing.T) (*Server, *db.GormDB) {
	server, database := newTestServer(t, "tester")
	server.cfg.GitHubAppClientID = "app"
	return server, database
}

func TestIsAdmin_NilUserIsDenied(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	assert.False(t, server.isAdmin(nil))
}

func TestIsAdmin_DevModeGrantsEveryone(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	assert.True(t, server.isAdmin(&db.User{GitHubUsername: "nobody"}))
}

func TestIsAdmin_EnvLoginGrantsCaseInsensitively(t *testing.T) {
	server, _ := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"alice"}

	assert.True(t, server.isAdmin(&db.User{GitHubUsername: "Alice"}))
	assert.False(t, server.isAdmin(&db.User{GitHubUsername: "bob"}))
}

func TestIsAdmin_SettingsRowLoginGrants(t *testing.T) {
	server, database := newNonDevTestServer(t)
	require.NoError(t, database.SetSetting(settingAdminLogins, "carol,Dave"))

	assert.True(t, server.isAdmin(&db.User{GitHubUsername: "carol"}))
	assert.True(t, server.isAdmin(&db.User{GitHubUsername: "dave"}))
	assert.False(t, server.isAdmin(&db.User{GitHubUsername: "erin"}))
}

func TestIsAdmin_StarInSettingsRowGrantsNobody(t *testing.T) {
	server, database := newNonDevTestServer(t)
	require.NoError(t, database.SetSetting(settingAdminLogins, "*"))

	assert.False(t, server.isAdmin(&db.User{GitHubUsername: "alice"}))
	assert.False(t, server.isAdmin(&db.User{GitHubUsername: "*"}))
}

func TestIsAdmin_SettingReadErrorDenies(t *testing.T) {
	server, database := newNonDevTestServer(t)
	require.NoError(t, database.SetSetting(settingAdminLogins, "alice"))
	server.db = settingReadFails{Database: database, key: settingAdminLogins}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	assert.False(t, server.isAdmin(&db.User{GitHubUsername: "alice"}))
	assert.Equal(t, 1, strings.Count(buf.String(), "\n"), buf.String())
	assert.Contains(t, buf.String(), settingAdminLogins)
}

func TestSplitLogins(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"Alice, bob,,alice", []string{"alice", "bob"}},
		{"bob,alice,Bob", []string{"bob", "alice"}},
		{"*, alice", []string{"*", "alice"}},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, splitLogins(tc.in), "input %q", tc.in)
	}
}

func TestNormalizeLoginCSV(t *testing.T) {
	long39 := strings.Repeat("a", 39)
	cases := []struct {
		name      string
		in        string
		allowStar bool
		want      string
		wantErr   string
	}{
		{"empty", "", false, "", ""},
		{"dedupes and lowercases", "Alice, bob,,alice", false, "alice,bob", ""},
		{"leading hyphen", "-a", false, "", `"-a" is not a valid login`},
		{"embedded space", "a b", false, "", `"a b" is not a valid login`},
		{"39 chars", long39, false, long39, ""},
		{"40 chars", long39 + "a", false, "", `"` + long39 + `a" is not a valid login`},
		{"star allowed", "Alice, *", true, "alice,*", ""},
		{"star refused", "alice,*", false, "", `"*" is not a valid login`},
		{"first bad entry named", "alice,al ice,-b", false, "", `"al ice" is not a valid login`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeLoginCSV(tc.in, tc.allowStar)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWriteSetting_LogsActorKeyOldNew(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("publish_reply_mode", "observe"))

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	require.NoError(t, server.writeSetting("alice", "publish_reply_mode", "react"))

	stored, _ := database.GetSetting("publish_reply_mode")
	assert.Equal(t, "react", stored)
	assert.Equal(t, 1, strings.Count(buf.String(), "\n"), buf.String())
	assert.Contains(t, buf.String(), `[SETTINGS] actor=alice key=publish_reply_mode old="observe" new="react"`)
}

func TestWriteSetting_RefusesToWriteWhenOldValueIsUnreadable(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("publish_reply_mode", "observe"))
	server.db = settingReadFails{Database: database, key: "publish_reply_mode"}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	assert.Error(t, server.writeSetting("alice", "publish_reply_mode", "react"))
	stored, _ := database.GetSetting("publish_reply_mode")
	assert.Equal(t, "observe", stored)
	assert.Empty(t, buf.String())
}

func TestWriteSetting_ReturnsWriteError(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.db = settingWriteFails{Database: database, key: "publish_reply_mode"}

	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stderr)

	assert.Error(t, server.writeSetting("alice", "publish_reply_mode", "react"))
	stored, _ := database.GetSetting("publish_reply_mode")
	assert.Equal(t, "", stored)
}
