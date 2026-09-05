package sdkgen

// collectNamed 深度优先收集具名类型（object/enum），去重后按后序排列：
// 嵌套类型排在引用它的父类型之前。Python 的 dataclass 注解在类体执行时求值，
// 父类型先输出会直接 NameError（Go/TS 与顺序无关）。三 emitter 共用。
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
			if current.Kind == KindObject {
				for _, field := range current.Fields {
					visit(field.Type)
				}
			}
			ordered = append(ordered, current)
			return
		}
		if current.Kind == KindArray {
			visit(current.Elem)
		}
	}
	visit(model)
	return ordered
}
