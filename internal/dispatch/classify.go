package dispatch

// Outcome 是一次投递尝试的分类结果（spec §5.2）。
type Outcome int

const (
	OutcomeSuccess   Outcome = iota // 2xx → succeeded
	OutcomeRetryable                // 网络错误 / 超时 / 408 / 425 / 429 / 5xx
	OutcomePermanent                // 其余 4xx、3xx（不跟随重定向）→ 立即 dead
)

// Classify 按 spec §5.2 对尝试结果分类。err 非 nil 表示网络层失败（含超时）。
func Classify(statusCode int, err error) Outcome {
	if err != nil {
		return OutcomeRetryable
	}
	switch {
	case statusCode >= 200 && statusCode < 300:
		return OutcomeSuccess
	case statusCode == 408 || statusCode == 425 || statusCode == 429:
		return OutcomeRetryable
	case statusCode >= 500:
		return OutcomeRetryable
	default:
		return OutcomePermanent
	}
}
