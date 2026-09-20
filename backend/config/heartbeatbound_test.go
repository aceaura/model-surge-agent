package config

import (
	"strings"
	"testing"
	"time"
)

// 判据 9：心跳比首帧超时还长时拒绝启动。
//
// 不拒的话进程正常起来、无任何告警，而长思考请求在第一个心跳发出之前就被首帧
// 超时掐断——中间设施与客户端看到的是连接静默断开，运维以为保活正开着。
// 这个不变量此前只存在于 pipeline/upstream.go 的注释里。
func TestHeartbeatLongerThanFirstTokenTimeoutFailsStartup(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "60s")
	t.Setenv("MSA_IDLE_TIMEOUT", "120s")
	t.Setenv("MSA_HEARTBEAT_INTERVAL", "90s")
	_, err := Load()
	if err == nil {
		t.Fatal("一个永远发不出心跳的配置被接受了")
	}
	for _, want := range []string{"MSA_HEARTBEAT_INTERVAL", "MSA_FIRST_TOKEN_TIMEOUT", "MSA_IDLE_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误没点名 %s：%v", want, err)
		}
	}
	// 三个值都要出现在文本里：只说「invalid」时运维不知道该调哪个、调到多少。
	for _, want := range []string{"1m30s", "1m0s", "2m0s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误没给出值 %s：%v", want, err)
		}
	}
}

// 判据 10：取两个超时的较小者，不是只比首帧。
//
// 空闲超时比心跳还短时，静默期里同样一个心跳都发不出。这条用「心跳短于首帧
// 但长于空闲」这一格，只比首帧的实现在这里会放行。
func TestHeartbeatLongerThanIdleTimeoutFailsStartup(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "120s")
	t.Setenv("MSA_IDLE_TIMEOUT", "20s")
	t.Setenv("MSA_HEARTBEAT_INTERVAL", "60s")
	if _, err := Load(); err == nil {
		t.Fatal("心跳长于空闲超时的配置被接受了；静默期里一个心跳都发不出")
	}
}

// 判据 11：恰好相等也拒。
//
// 相等时第一个心跳与超时同刻到达，谁先取决于调度——这种配置没有存在的理由，
// 而它恰好是「>」与「>=」的唯一分界，不钉住就分不出实现用了哪个。
func TestHeartbeatEqualToTimeoutFailsStartup(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "30s")
	t.Setenv("MSA_IDLE_TIMEOUT", "30s")
	t.Setenv("MSA_HEARTBEAT_INTERVAL", "30s")
	if _, err := Load(); err == nil {
		t.Fatal("心跳恰好等于超时的配置被接受了")
	}
}

// 判据 12：显式关闭（<= 0）跳过联合校验。
//
// 负值表达「关掉保活」是既有的合法配置，联合校验不能把它连带拒掉。
// 零值同理。
func TestDisabledHeartbeatSkipsTheJointCheck(t *testing.T) {
	for name, v := range map[string]string{"负值": "-1s", "零": "0"} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			// 这组超时配得比心跳的绝对值还小，如果关闭态也走联合校验就会被拒。
			t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "500ms")
			t.Setenv("MSA_IDLE_TIMEOUT", "500ms")
			t.Setenv("MSA_HEARTBEAT_INTERVAL", v)
			c, err := Load()
			if err != nil {
				t.Fatalf("关闭保活的配置被拒了：%v", err)
			}
			if c.HeartbeatInterval > 0 {
				t.Errorf("HeartbeatInterval = %s，want <= 0", c.HeartbeatInterval)
			}
		})
	}
}

// 判据 13：合法配置仍能启动，且值被如实读进来。
//
// 反面用例：校验写成恒拒时上面三条全绿，只有这条会红。
func TestValidHeartbeatIsAccepted(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "60s")
	t.Setenv("MSA_IDLE_TIMEOUT", "120s")
	t.Setenv("MSA_HEARTBEAT_INTERVAL", "15s")
	c, err := Load()
	if err != nil {
		t.Fatalf("合法配置被拒了：%v", err)
	}
	if c.HeartbeatInterval != 15*time.Second {
		t.Errorf("HeartbeatInterval = %s，want 15s", c.HeartbeatInterval)
	}
}

// 判据 14：默认值本身必须满足这个不变量。
//
// 默认 15s / 60s / 120s。不钉的话把某个默认值改成矛盾的一组时，一个不设任何
// 环境变量的部署会直接起不来——而那正是最常见的部署形态。
func TestDefaultHeartbeatSatisfiesTheInvariant(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("默认配置起不来：%v", err)
	}
	if c.HeartbeatInterval <= 0 {
		t.Fatalf("默认心跳 = %s，本该是正数", c.HeartbeatInterval)
	}
	if c.HeartbeatInterval >= c.FirstTokenTimeout || c.HeartbeatInterval >= c.IdleTimeout {
		t.Errorf("默认值互相矛盾：心跳 %s，首帧 %s，空闲 %s",
			c.HeartbeatInterval, c.FirstTokenTimeout, c.IdleTimeout)
	}
}
