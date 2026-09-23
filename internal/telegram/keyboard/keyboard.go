package keyboard

func Main() map[string]any {
	return map[string]any{
		"keyboard": [][]map[string]string{
			{{"text": "🤖 Agent"}, {"text": "📊 Context"}},
			{{"text": "🧠 Model"}, {"text": "💭 Variant"}},
		},
		"resize_keyboard": true, "is_persistent": true,
	}
}
