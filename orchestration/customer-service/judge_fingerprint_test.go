package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// 候选 C1 的内容指纹：rubric 与提示词模板按场景排序离线渲染，模型配置按名称序列化，两者都用 json.MarshalIndent(v, "", "  ")、末尾不加换行。
// 渲染结果与 review/2026-11-18-02-closeout/candidate/ 下的 rubrics-and-prompts.json、model-configs.json 逐字节相同。
// 验收入口在第一次请求之前重新渲染并比对，不一致就不运行。
var candidateC1 = candidateFingerprint{
	RubricsAndPrompts: "c3757ad5d058df677eeebfc08936da29d62036303bf1efaa976575bd6f1b134b",
	ModelConfigs:      "a3c001cf9da78a10f3fc078d7411f11507848729c8cc85559cccc184407946cb",
	ConfigHash: map[string]string{
		"deepseek": "d14fdf517fac1d60ad2b269c5c8779a47305e56bd3b2f5c8af0fdb9b0e2d504a",
		"gemini":   "5eadeca019275797ed62f05c6fcf240bd3e0458bf62f879b4d8c87d23075b84d",
	},
	Versions: map[string]string{"rubric": "rubric/4", "prompt": "checklist-prompt/2", "schema": "checks-schema/2",
		"executor": "judge-executor/5", "record": "judge-record/3"},
	Positions:  3,
	MaxRetries: 2,
}

type candidateFingerprint struct {
	RubricsAndPrompts string            `json:"rubrics_and_prompt_templates_sha256"`
	ModelConfigs      string            `json:"model_configs_sha256"`
	ConfigHash        map[string]string `json:"model_config_json_sha256"`
	Versions          map[string]string `json:"versions"`
	Positions         int               `json:"positions"`
	MaxRetries        int               `json:"max_retries"`
}

// renderCandidate 返回当前代码的两份渲染产物与指纹。提示词里的问题、场景说明、答复替换为占位符，只反映模板本身。
func renderCandidate() (rubricsAndPrompts, modelConfigs []byte, fp candidateFingerprint, err error) {
	type rendered struct {
		Scenario string
		R        judgeRubric
		Prompt   string
	}
	var rs []rendered
	for _, name := range slices.Sorted(maps.Keys(judgeRubrics)) {
		r := judgeRubrics[name]
		rs = append(rs, rendered{name, r, checklistPrompt(r, judgeInput{Question: "{question}", Expect: "{expect}", Reply: "{reply}"})})
	}
	if rubricsAndPrompts, err = json.MarshalIndent(rs, "", "  "); err != nil {
		return nil, nil, fp, err
	}
	if modelConfigs, err = json.MarshalIndent(judgeModelConfigs, "", "  "); err != nil {
		return nil, nil, fp, err
	}
	fp = candidateFingerprint{
		RubricsAndPrompts: sha256Hex(rubricsAndPrompts),
		ModelConfigs:      sha256Hex(modelConfigs),
		ConfigHash:        map[string]string{},
		Versions: map[string]string{"rubric": judgeRubrics["forget"].Version, "prompt": judgePromptVersion, "schema": judgeSchemaVersion,
			"executor": judgeExecutorVersion, "record": judgeRecordVersion},
		Positions:  judgePositions,
		MaxRetries: judgeMaxRetries,
	}
	for name, c := range judgeModelConfigs {
		fp.ConfigHash[name] = c.hash()
	}
	return rubricsAndPrompts, modelConfigs, fp, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// verifyCandidate 比对当前代码渲染的指纹与 want；参与运行的每个模型的配置哈希也必须与 want 一致。
func verifyCandidate(want candidateFingerprint, models []runModel) (candidateFingerprint, error) {
	_, _, got, err := renderCandidate()
	if err != nil {
		return got, err
	}
	for _, d := range []struct{ name, got, want string }{
		{"rubric 与提示词模板", got.RubricsAndPrompts, want.RubricsAndPrompts},
		{"模型配置合集", got.ModelConfigs, want.ModelConfigs},
	} {
		if d.got != d.want {
			return got, fmt.Errorf("%s指纹不符：%s ≠ 候选 %s", d.name, d.got, d.want)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(want.Versions)) {
		if got.Versions[k] != want.Versions[k] {
			return got, fmt.Errorf("%s 版本不符：%s ≠ 候选 %s", k, got.Versions[k], want.Versions[k])
		}
	}
	if got.Positions != want.Positions || got.MaxRetries != want.MaxRetries {
		return got, fmt.Errorf("采样位置或重试上限不符：%d/%d ≠ 候选 %d/%d", got.Positions, got.MaxRetries, want.Positions, want.MaxRetries)
	}
	for _, m := range models {
		h, ok := want.ConfigHash[m.Name]
		if !ok {
			return got, fmt.Errorf("模型 %s 不在候选里", m.Name)
		}
		if m.Model.Config.hash() != h {
			return got, fmt.Errorf("模型 %s 的配置哈希不符：%s ≠ 候选 %s", m.Name, m.Model.Config.hash(), h)
		}
	}
	return got, nil
}

// TestCandidateFingerprint 离线重新渲染候选 C1 并核对指纹；设置 CS_JUDGE_FINGERPRINT_OUT 时把渲染产物写到该新目录，供逐字对比。
func TestCandidateFingerprint(t *testing.T) {
	rp, mc, fp, err := renderCandidate()
	require.NoError(t, err)
	_, err = verifyCandidate(candidateC1, nil)
	require.NoError(t, err)
	require.Equal(t, candidateC1, fp)

	out := os.Getenv("CS_JUDGE_FINGERPRINT_OUT")
	if out == "" {
		return
	}
	require.NoError(t, os.Mkdir(out, 0o755), "输出目录必须是新目录")
	// fingerprint.json 按键名排序输出，与归档文件格式一致。
	b, err := json.Marshal(fp)
	require.NoError(t, err)
	var sorted map[string]any
	require.NoError(t, json.Unmarshal(b, &sorted))
	fb, err := json.MarshalIndent(sorted, "", "  ")
	require.NoError(t, err)
	for name, b := range map[string][]byte{"rubrics-and-prompts.json": rp, "model-configs.json": mc, "fingerprint.json": fb} {
		require.NoError(t, writeExclusive(filepath.Join(out, name), b))
	}
}

func TestVerifyCandidateRejects(t *testing.T) {
	ds := runModel{Name: "deepseek", Model: judgeModel{Config: judgeModelConfigs["deepseek"]}}
	tweaked := judgeModelConfigs["deepseek"]
	tweaked.MaxOutputTokens = 4096
	tests := []struct {
		name   string
		mutate func(*candidateFingerprint)
		models []runModel
		errSub string
	}{
		{"一致", func(*candidateFingerprint) {}, []runModel{ds}, ""},
		{"提示词指纹不符", func(f *candidateFingerprint) { f.RubricsAndPrompts = "x" }, nil, "rubric 与提示词模板"},
		{"配置合集指纹不符", func(f *candidateFingerprint) { f.ModelConfigs = "x" }, nil, "模型配置合集"},
		{"版本不符", func(f *candidateFingerprint) { f.Versions = map[string]string{"executor": "judge-executor/4"} }, nil, "executor 版本不符"},
		{"重试上限不符", func(f *candidateFingerprint) { f.MaxRetries = 3 }, nil, "重试上限"},
		{"运行配置被改", func(*candidateFingerprint) {}, []runModel{{Name: "deepseek", Model: judgeModel{Config: tweaked}}}, "配置哈希不符"},
		{"候选外的模型", func(*candidateFingerprint) {}, []runModel{{Name: "other", Model: judgeModel{Config: tweaked}}}, "不在候选里"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := candidateC1
			want.ConfigHash = maps.Clone(candidateC1.ConfigHash)
			want.Versions = maps.Clone(candidateC1.Versions)
			tt.mutate(&want)
			_, err := verifyCandidate(want, tt.models)
			if tt.errSub == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.errSub)
		})
	}
}
