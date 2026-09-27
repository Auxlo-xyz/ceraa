package xaiauth

import "os"

func liveDisabled() bool { return os.Getenv("CERAA_LIVE_OAUTH") != "1" }
