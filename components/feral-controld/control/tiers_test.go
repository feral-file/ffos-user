package control

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// commandConstants returns every string constant of type commands.Type
// declared in commands/types.go, read from source so a new constant is
// picked up without anyone remembering to list it here.
func commandConstants(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../commands/types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Type" {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a literal", name.Name)
				}
				v, _ := strconv.Unquote(lit.Value)
				out[name.Name] = v
			}
		}
	}
	if len(out) < 40 {
		t.Fatalf("parsed only %d command constants; parser out of date?", len(out))
	}
	return out
}

// playerCommands is the ff-player CDP surface a controld command can fall
// through to: CastCommand in src/models/cast_info.model.ts plus the
// CDPRequestHandler.ts special cases (ping, mintPairingDisplay,
// setupDisplay, get/setContentPolicy), read at ff-player main @41b266f.
// When the player adds a command, add it here AND to commandTiers.
var playerCommands = []string{
	"connect", "disconnect", "checkStatus", "moveToArtwork", "updateIndex", "refreshArtwork",
	"updateArtFraming", "updateDisplaySettings", "cursorUpdate", "displayPlaylist", "refreshPlaylist",
	"setSleepMode", "setShuffle", "setLoop", "displayDefaultPlaylist", "updateDefaultDuration",
	"getRecentlyPlayed", "resolveRecentlyPlayed",
	"ping", "mintPairingDisplay", "setupDisplay", "getContentPolicy", "setContentPolicy",
}

func TestEveryCommandHasATier(t *testing.T) {
	for name, cmd := range commandConstants(t) {
		if _, ok := commandTiers[cmd]; !ok {
			t.Errorf("commands.%s (%q) has no tier in commandTiers (§7.1)", name, cmd)
		}
	}
	for _, cmd := range playerCommands {
		if _, ok := commandTiers[cmd]; !ok {
			t.Errorf("player command %q has no tier", cmd)
		}
	}
	// String literals commandrouter compares command types against.
	src, err := os.ReadFile("../commandrouter/handler.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`[Tt]ype == "([A-Za-z]+)"`).FindAllSubmatch(src, -1) {
		if _, ok := commandTiers[string(m[1])]; !ok {
			t.Errorf("commandrouter literal %q has no tier", m[1])
		}
	}
}

func TestTierTableHasNoPhantoms(t *testing.T) {
	known := map[string]bool{CmdApproveController: true, CmdRevokeController: true, CmdListControllers: true,
		CmdRegisterController: true, CmdEnvelope: true}
	for _, v := range commandConstants(t) {
		known[v] = true
	}
	for _, v := range playerCommands {
		known[v] = true
	}
	for cmd := range commandTiers {
		if !known[cmd] {
			t.Errorf("commandTiers lists %q, which no command source defines", cmd)
		}
	}
}

func TestTierOfAndAllows(t *testing.T) {
	if TierOf("noSuchCommand") != TierOwner || TierOf("sshAccess") != TierOwner ||
		TierOf("displayPlaylist") != TierController || TierOf("ping") != TierInternal {
		t.Fatal("TierOf")
	}
	for _, tc := range []struct {
		tier Tier
		role Role
		ok   bool
	}{
		{TierController, RoleController, true}, {TierController, RoleOwner, true},
		{TierOwner, RoleController, false}, {TierOwner, RoleOwner, true},
		{TierInternal, RoleOwner, false}, {TierInternal, RoleController, false},
		{TierOwner, "", false},
	} {
		if tc.tier.Allows(tc.role) != tc.ok {
			t.Errorf("%s.Allows(%s) != %v", tc.tier, tc.role, tc.ok)
		}
	}
}
