package ir

import "testing"

// container 在 IR 层的贯通：ResponseEvents 把完整响应投影成事件时，容器回显
// 随首事件走；Aggregator 反向聚合时后值覆盖（message_delta 晚到的更完整）。
//
// 对应旧仓 #31（2d9fc72）。

func TestResponseEventsCarriesContainer(t *testing.T) {
	resp := &Response{ID: "m", Model: "m",
		Container: &Container{ID: "ctr_1", ExpiresAt: "t1"}}
	evs := ResponseEvents(resp)
	if len(evs) == 0 || evs[0].Container == nil || evs[0].Container.ID != "ctr_1" {
		t.Fatalf("首事件未带 container：%+v", evs)
	}
}

func TestAggregatorContainerLaterWins(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart, MessageID: "m",
		Container: &Container{ID: "ctr_s", ExpiresAt: "t1"}})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopEndTurn,
		Container: &Container{ID: "ctr_s", ExpiresAt: "t2",
			Skills: []Skill{{SkillID: "s1", Type: "anthropic", Version: "v1"}}}})
	got := a.Response()
	if got.Container == nil || got.Container.ExpiresAt != "t2" || len(got.Container.Skills) != 1 {
		t.Fatalf("聚合后值覆盖错：%+v", got.Container)
	}
}

// 缺 container 的帧不该把已收到的容器清零。
func TestAggregatorContainerNotClearedByAbsentFrame(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart, MessageID: "m",
		Container: &Container{ID: "ctr_only", ExpiresAt: "t0"}})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopEndTurn})
	got := a.Response()
	if got.Container == nil || got.Container.ID != "ctr_only" {
		t.Fatalf("首帧容器被后续空帧清零：%+v", got.Container)
	}
}

// Clone 必须深拷贝 Container，换目标重试时两份请求不共享技能切片。
func TestRequestCloneDeepCopiesContainer(t *testing.T) {
	r := &Request{Model: "m", Container: &Container{ID: "ctr_1",
		Skills: []Skill{{SkillID: "s1", Type: "custom"}}}}
	c := r.Clone()
	if c.Container == r.Container {
		t.Fatal("Clone 未深拷贝 Container 指针")
	}
	if len(c.Container.Skills) != 1 || &c.Container.Skills[0] == &r.Container.Skills[0] {
		t.Fatal("Clone 未深拷贝 Skills 切片")
	}
	c.Container.Skills[0].Version = "v9"
	if r.Container.Skills[0].Version == "v9" {
		t.Error("改动克隆体的技能串到了原请求")
	}
}
