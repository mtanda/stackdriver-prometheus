// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package relabel

import (
	"crypto/md5"
	"fmt"
	"regexp"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/pkg/labels"
)

// Process returns a relabeled copy of the given label set. The relabel configurations
// are applied in order of input.
// If a label set is dropped, nil is returned.
// May return the input labelSet modified.
func Process(labels labels.Labels, cfgs ...*config.RelabelConfig) labels.Labels {
	for _, cfg := range cfgs {
		labels = relabel(labels, cfg)
		if labels == nil {
			return nil
		}
	}
	return labels
}

func relabel(lset labels.Labels, cfg *config.RelabelConfig) labels.Labels {
	values := make([]string, 0, len(cfg.SourceLabels))
	for _, ln := range cfg.SourceLabels {
		values = append(values, lset.Get(string(ln)))
	}
	val := strings.Join(values, cfg.Separator)

	lb := labels.NewBuilder(lset)

	var regex *regexp.Regexp
	if cfg.Regex.Initialized() {
		regex = cfg.Regex.Pool.Get().(*regexp.Regexp)
		if regex == nil {
			panic(fmt.Sprintf("%v", cfg))
		}
	}
	defer func() {
		if regex != nil {
			cfg.Regex.Pool.Put(regex)
		}
	}()
	switch cfg.Action {
	case config.RelabelDrop:
		if regex.MatchString(val) {
			return nil
		}
	case config.RelabelKeep:
		if !regex.MatchString(val) {
			return nil
		}
	case config.RelabelReplace:
		indexes := regex.FindStringSubmatchIndex(val)
		// If there is no match no replacement must take place.
		if indexes == nil {
			break
		}
		target := model.LabelName(regex.ExpandString([]byte{}, cfg.TargetLabel, val, indexes))
		if !target.IsValid() {
			lb.Del(cfg.TargetLabel)
			break
		}
		res := regex.ExpandString([]byte{}, cfg.Replacement, val, indexes)
		if len(res) == 0 {
			lb.Del(cfg.TargetLabel)
			break
		}
		lb.Set(string(target), string(res))
	case config.RelabelHashMod:
		mod := sum64(md5.Sum([]byte(val))) % cfg.Modulus
		lb.Set(cfg.TargetLabel, fmt.Sprintf("%d", mod))
	case config.RelabelLabelMap:
		for _, l := range lset {
			if regex.MatchString(l.Name) {
				res := regex.ReplaceAllString(l.Name, cfg.Replacement)
				lb.Set(res, l.Value)
			}
		}
	case config.RelabelLabelDrop:
		for _, l := range lset {
			if regex.MatchString(l.Name) {
				lb.Del(l.Name)
			}
		}
	case config.RelabelLabelKeep:
		for _, l := range lset {
			if !regex.MatchString(l.Name) {
				lb.Del(l.Name)
			}
		}
	default:
		panic(fmt.Errorf("retrieval.relabel: unknown relabel action type %q", cfg.Action))
	}

	return lb.Labels()
}

// sum64 sums the md5 hash to an uint64.
func sum64(hash [md5.Size]byte) uint64 {
	var s uint64

	for i, b := range hash {
		shift := uint64((md5.Size - i - 1) * 8)

		s |= uint64(b) << shift
	}
	return s
}
