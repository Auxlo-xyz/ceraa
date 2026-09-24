package keyboard

func Main() map[string]any {
	return map[string]any{
		"keyboard": [][]map[string]string{
			{{"text": "🤖 Agent"}, {"text": "📊 Context"}},
			{{"text": "📋 Menu"}, {"text": "💭 Variant"}},
		},
		"resize_keyboard": true, "is_persistent": true,
	}
}
