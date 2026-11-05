package lifecycle

// DigestSuperseded is the event Component digest superseded (ST-DIS-EV03). Its durable form is the advisory that
// Supersede writes into the lifecycle folder of the registry; a consumer reads it at its next run from the
// registry's head.
type DigestSuperseded struct {
	Subject      Subject
	Supersession Supersession
}

// DigestWithdrawn is the event Component digest withdrawn (ST-DIS-EV04). Its durable form is the advisory that
// Withdraw writes into the lifecycle folder of the registry.
type DigestWithdrawn struct {
	Subject    Subject
	Withdrawal Withdrawal
}
