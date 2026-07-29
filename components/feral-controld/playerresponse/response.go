package playerresponse

// OK checks whether the CDP result from the player reports ok:true. Most
// player commands return { "message": { "ok": true } }; a few older paths
// return top-level { "ok": true }.
func OK(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		okVal, _ := msg["ok"].(bool)
		return okVal
	}
	okVal, _ := m["ok"].(bool)
	return okVal
}
