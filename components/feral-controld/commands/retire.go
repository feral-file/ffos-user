package commands

// MarkRetirement turns displayPlaylist arguments into a retirement: the player
// takes the on-screen work down at once, then shows whatever the accompanying
// list still allows, or nothing when it is empty.
//
// The two keys must travel together, and this is the only place that writes
// them. ff-player reads retireBlockedCurrent only on the path refresh:true
// selects; without it the command is an ordinary display request, which
// refuses an empty list (the blocked work stays up) and keeps the outgoing work
// on screen until a non-empty list's first work commits
// (feral-file/ffos-user#349 shipped three senders that way). On that path the
// player ignores intent, playlistUrl and startItem, and keeps the playlistUrl
// of the cast it is retiring from.
func MarkRetirement(arguments map[string]any) {
	arguments["refresh"] = true
	arguments["retireBlockedCurrent"] = true
}
