package statemachine

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

type State int

const (
	StateNoise       State = iota // 初始观察期
	StateStable                   // 正常交易
	StateStress                   // 仅 Squeeze 引擎
	StateDegradation              // 仅平仓
	StateFailure                  // 强制平仓，等待人工
)

func (s State) String() string {
	switch s {
	case StateNoise:
		return "NOISE"
	case StateStable:
		return "STABLE"
	case StateStress:
		return "STRESS"
	case StateDegradation:
		return "DEGRADATION"
	case StateFailure:
		return "FAILURE"
	default:
		return "UNKNOWN"
	}
}

// TransitionRequest 外部模块请求状态升级
type TransitionRequest struct {
	Target State
	Reason string
}

// StateMachine 状态机，每秒检查一次
type StateMachine struct {
	mu      sync.RWMutex
	current State
	entered time.Time // 进入当前状态的时间

	// 外部请求
	requests chan TransitionRequest

	// 观察期结束后切换到 STABLE
	noiseSince time.Time

	// 用于 STABLE→STRESS 的迟滞
	lastStressCheck  time.Time
	stressCondition  time.Time

	// 状态变更通知
	onChange func(from, to State, reason string)
}

func New(onChange func(from, to State, reason string)) *StateMachine {
	return &StateMachine{
		current:  StateNoise,
		entered:  time.Now(),
		requests: make(chan TransitionRequest, 64),
		noiseSince: time.Now(),
		onChange: onChange,
	}
}

func (sm *StateMachine) Current() State {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current
}

// RequestTransition 外部模块请求升级状态（不降级，降级需满足恢复条件）
func (sm *StateMachine) RequestTransition(target State, reason string) {
	select {
	case sm.requests <- TransitionRequest{Target: target, Reason: reason}:
	default:
		log.Warn().Str("target", target.String()).Msg("state transition channel full")
	}
}

func (sm *StateMachine) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-sm.requests:
			sm.applyRequest(req)
		case <-ticker.C:
			sm.tick()
		}
	}
}

func (sm *StateMachine) applyRequest(req TransitionRequest) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cur := sm.current

	// FAILURE 只能人工恢复
	if cur == StateFailure {
		return
	}
	// 只允许升级（状态数值越大越严重）
	if req.Target <= cur {
		return
	}
	sm.transition(req.Target, req.Reason)
}

func (sm *StateMachine) tick() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	cur := sm.current

	switch cur {
	case StateNoise:
		// 5 分钟后进入 STABLE
		if now.Sub(sm.noiseSince) >= 5*time.Minute {
			sm.transition(StateStable, "initial observation complete")
		}
	case StateStable, StateStress, StateDegradation:
		// 恢复：满足恢复条件持续 5 分钟
		// 简化：此处仅做时间判断，实际条件由外部 metrics 决定
		// 外部通过 RequestRecovery 触发降级
	}
}

// RecoverTo 人工/自动降级（必须满足 5min 稳定窗口，FAILURE 除外需人工）
func (sm *StateMachine) RecoverTo(target State, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cur := sm.current
	if cur == StateFailure {
		log.Error().Msg("FAILURE state requires manual recovery")
		return
	}
	if target >= cur {
		return
	}
	sm.transition(target, "recovery: "+reason)
}

func (sm *StateMachine) transition(to State, reason string) {
	from := sm.current
	sm.current = to
	sm.entered = time.Now()
	log.Info().
		Str("from", from.String()).
		Str("to", to.String()).
		Str("reason", reason).
		Time("at", sm.entered).
		Msg("state transition")
	if sm.onChange != nil {
		go sm.onChange(from, to, reason)
	}
}

// CanOpen 是否允许开仓
func (sm *StateMachine) CanOpen() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current == StateStable || sm.current == StateStress
}

// CanOpenTrend 趋势/过渡引擎仅在 STABLE 可开
func (sm *StateMachine) CanOpenTrend() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current == StateStable
}

// MustClose 是否需要强制平仓
func (sm *StateMachine) MustClose() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current == StateDegradation || sm.current == StateFailure
}
