package alerts

import "github.com/harishhary/blink/internal/messaging"

type AlertMessage struct {
	messaging.Message
	Alert Alert
}
