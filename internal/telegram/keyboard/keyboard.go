package keyboard

func Main() map[string]any {
	return map[string]any{
		"keyboard": [][]map[string]string{
			{{"text": "🤖 Mode"}, {"text": "📊 Context"}},
			{{"text": "📋 Hub"}, {"text": "💭 Variant"}},
		},
		"resize_keyboard": true, "is_persistent": true,
	}
}
