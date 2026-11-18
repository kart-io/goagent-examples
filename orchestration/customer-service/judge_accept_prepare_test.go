package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeAcceptThresholds 用验收入口的同一套计算写出 dir 下的 acceptance-thresholds.json（独占创建），避免手算不一致。
func writeAcceptThresholds(set, dir string) ([]byte, error) {
	spec, ok := acceptSets[set]
	if !ok {
		return nil, fmt.Errorf("未知的验收数据 %q", set)
	}
	items, err := loadLabelFile(filepath.Join(dir, spec.Data), spec.Format)
	if err != nil {
		return nil, err
	}
	if spec.Pairs != "" {
		if _, err := loadPairs(filepath.Join(dir, spec.Pairs), items); err != nil {
			return nil, err
		}
	}
	th := thresholdsFor(set, items)
	if set == "holdout" {
		if err := holdoutSufficient(th.Base); err != nil {
			return nil, err
		}
	}
	b, err := json.MarshalIndent(th, "", "  ")
	if err != nil {
		return nil, err
	}
	return b, writeExclusive(filepath.Join(dir, acceptThresholdsFile), b)
}

// dryPrepareAcceptance 对冻结目录做一次不发请求的就绪检查；模型只取候选配置，不创建客户端。
func dryPrepareAcceptance(set, dir string) (acceptInput, error) {
	var models []runModel
	for _, name := range acceptModels {
		models = append(models, runModel{Name: name, Model: judgeModel{Config: judgeModelConfigs[name]}})
	}
	return prepareAcceptance(set, dir, models)
}

// TestAcceptanceThresholdsOut：CS_JUDGE_THRESHOLDS_SET=holdout|challenge，CS_JUDGE_THRESHOLDS_DATA=<待冻结目录>。
func TestAcceptanceThresholdsOut(t *testing.T) {
	set, dir := os.Getenv("CS_JUDGE_THRESHOLDS_SET"), os.Getenv("CS_JUDGE_THRESHOLDS_DATA")
	if set == "" || dir == "" {
		t.Skip("设置 CS_JUDGE_THRESHOLDS_SET 与 CS_JUDGE_THRESHOLDS_DATA 以写出整数容许数")
	}
	b, err := writeAcceptThresholds(set, dir)
	require.NoError(t, err)
	t.Logf("%s", b)
}

// TestAcceptanceDryPrepare：CS_JUDGE_ACCEPT_DRY=1，CS_JUDGE_ACCEPT_SET 与 CS_JUDGE_ACCEPT_DATA 同验收入口。
func TestAcceptanceDryPrepare(t *testing.T) {
	set, data := os.Getenv("CS_JUDGE_ACCEPT_SET"), os.Getenv("CS_JUDGE_ACCEPT_DATA")
	if set == "" || data == "" || os.Getenv("CS_JUDGE_ACCEPT_DRY") != "1" {
		t.Skip("设置 CS_JUDGE_ACCEPT_DRY=1、CS_JUDGE_ACCEPT_SET 与 CS_JUDGE_ACCEPT_DATA 以做离线就绪检查")
	}
	in, err := dryPrepareAcceptance(set, data)
	require.NoError(t, err)
	t.Logf("%s 就绪：评审 %d 条，跳过无答复 %d 条，N=%d Nc=%d Ne=%d uncertain=%d", set, len(in.Phase.Items), len(in.Skipped),
		in.Th.Base.N, in.Th.Base.Nc, in.Th.Base.Ne, in.Th.Base.Uncertain)
}

func TestWriteAcceptThresholdsAndDryPrepare(t *testing.T) {
	for _, set := range []string{"holdout", "challenge"} {
		t.Run(set, func(t *testing.T) {
			f := newAcceptFixture(set)
			want, err := os.ReadFile(filepath.Join(f.write(t, set, nil), acceptThresholdsFile))
			require.NoError(t, err)
			dir := f.write(t, set, nil)
			require.NoError(t, os.Remove(filepath.Join(dir, acceptThresholdsFile)))
			got, err := writeAcceptThresholds(set, dir)
			require.NoError(t, err)
			require.Equal(t, string(want), string(got), "与入口冻结时的计算逐字节相同")
			_, err = writeAcceptThresholds(set, dir)
			require.Error(t, err, "已存在时不覆盖")

			_, err = dryPrepareAcceptance(set, f.write(t, set, nil))
			require.NoError(t, err)
		})
	}
}
