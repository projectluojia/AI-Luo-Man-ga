package sdkgen

// collectNamed 深度优先收集具名类型（object/enum），按出现顺序去重。
// 三 emitter（Go/Python/TS）共用。
func collectNamed(model *TypeModel) []*TypeModel {
	var ordered []*TypeModel
	seen := make(map[string]struct{})
	var visit func(*TypeModel)
	visit = func(current *TypeModel) {
		if current == nil {
			return
		}
		if current.Kind == KindObject || current.Kind == KindEnum {
			if _, exists := seen[current.Name]; exists {
				return
			}
			seen[current.Name] = struct{}{}
			ordered = append(ordered, current)
			if current.Kind == KindObject {
				for _, field := range current.Fields {
					visit(field.Type)
				}
			}
			return
		}
		if current.Kind == KindArray {
			visit(current.Elem)
		}
	}
	visit(model)
	return ordered
}

// hasKind 判断类型树中是否存在指定 kind（Python 导入判断用，TS 未来可能用）。
func hasKind(model *TypeModel, kind TypeKind) bool {
	if model == nil {
		return false
	}
	if model.Kind == kind {
		return true
	}
	if model.Kind == KindArray {
		return hasKind(model.Elem, kind)
	}
	if model.Kind == KindObject {
		for _, field := range model.Fields {
			if hasKind(field.Type, kind) {
				return true
			}
		}
	}
	return false
}
