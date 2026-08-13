package acp

// Unavailable reports whether this client needs a replacement adapter process.
func (c *Client) Unavailable() bool {
	if c.retired.Load() || (c.conn != nil && c.conn.isClosed()) {
		return true
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
