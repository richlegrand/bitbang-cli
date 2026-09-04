package signaling

// Versions reads the latest-release table off a signaling-server message.
//
// The table is the newest published release of each BitBang client
// project, keyed by product ("cli", "octoprint"). Every recipient is sent
// the same table and looks up its own row, so a client never has to say
// what it is or what version it runs -- which is the point of the server
// answering at all rather than clients polling GitHub.
//
// The table arrives as untyped JSON, and it reaches both sides of a
// session: the listener gets it on the `registered` reply, the connector
// on the `hello` that opens its socket. One reader for both, because a
// silently-failed assertion here means nobody is ever told about an
// update, and that is not a failure anything else would notice.
//
// Returns nil for a message without a table, a table of the wrong shape,
// and a table whose every row is unusable -- all of which are the same
// thing to a caller: nothing to say.
func Versions(msg Message) map[string]string {
	raw, ok := msg["versions"].(map[string]interface{})
	if !ok {
		return nil
	}
	versions := make(map[string]string, len(raw))
	for product, latest := range raw {
		if s, ok := latest.(string); ok {
			versions[product] = s
		}
	}
	if len(versions) == 0 {
		return nil
	}
	return versions
}
