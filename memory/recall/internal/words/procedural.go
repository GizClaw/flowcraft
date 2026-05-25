package words

import (
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/phrase"
)

var proceduralVerbs = []string{
	"use", "check", "run", "call", "format", "respond",
	"return", "ask", "extract", "parse",
	"usar", "revisar", "ejecutar", "llamar", "formatear", "responder", "devolver", "preguntar", "extraer", "analizar",
	"utiliser", "vérifier", "verifier", "exécuter", "executer", "appeler", "formater", "répondre", "repondre", "retourner", "demander", "extraire", "analyser",
	"verwenden", "prüfen", "prufen", "ausführen", "ausfuhren", "aufrufen", "formatieren", "antworten", "zurückgeben", "zuruckgeben", "fragen", "extrahieren", "parsen",
	"usar", "verificar", "executar", "chamar", "formatar", "responder", "retornar", "perguntar", "extrair", "analisar",
	"gebruik", "controleren", "uitvoeren", "aanroepen", "formatteren", "beantwoorden", "teruggeven", "vragen", "extraheren", "parsen",
	"использовать", "проверить", "запустить", "вызвать", "форматировать", "ответить", "вернуть", "спросить", "извлечь", "разобрать",
}

var proceduralPreferenceTargets = []string{
	"markdown", "table", "format", "output", "response", "answer",
	"tabla", "formato", "salida", "respuesta",
	"tableau", "format", "sortie", "réponse", "reponse",
	"tabelle", "format", "ausgabe", "antwort",
	"tabela", "formato", "saída", "saida", "resposta",
	"tabel", "formaat", "uitvoer", "antwoord",
	"таблица", "формат", "вывод", "ответ",
	"表格", "格式", "输出", "回答", "答案",
}

var proceduralLeadingConditionTokens = [][]string{
	{"when"}, {"before"},
	{"cuando"}, {"cuándo"}, {"antes"},
	{"quand"}, {"avant"},
	{"wenn"}, {"bevor"},
	{"quando"}, {"antes"},
	{"wanneer"}, {"voordat"},
	{"когда"}, {"перед"},
}

var proceduralAlwaysTokens = []string{
	"always", "siempre", "toujours", "immer", "sempre", "altijd", "всегда",
}

var proceduralPreferenceTokens = []string{
	"prefer", "prefiere", "preferir", "préfère", "prefere", "bevorzugen", "bevorzugt",
	"voorkeur", "предпочитать", "предпочитает",
}

var proceduralFirstThenPairs = [][2]string{
	{"first", "then"},
	{"primero", "luego"},
	{"d'abord", "ensuite"},
	{"zuerst", "dann"},
	{"primeiro", "depois"},
	{"eerst", "dan"},
	{"сначала", "затем"},
}

var proceduralLiterals = []string{
	"总是", "始终", "先", "然后", "之前", "更喜欢", "优先",
}

func LooksProcedural(content string) bool {
	phrases := phrase.New(content)
	if len(phrases.Tokens()) == 0 {
		return false
	}
	for _, cue := range proceduralLeadingConditionTokens {
		if phrases.StartsWithPhrase(cue...) && strings.Contains(content, ",") {
			return true
		}
	}
	if strings.HasPrefix(strings.TrimSpace(content), "当") || strings.HasPrefix(strings.TrimSpace(content), "在") {
		if strings.Contains(content, "，") || strings.Contains(content, ",") {
			return true
		}
	}
	for _, pair := range proceduralFirstThenPairs {
		if phrases.Contains(pair[0]) && phrases.Contains(pair[1]) {
			return true
		}
	}
	if phrases.ContainsAnyLiteral(proceduralLiterals...) && strings.Contains(content, "然后") {
		return true
	}
	for _, always := range proceduralAlwaysTokens {
		if !phrases.Contains(always) {
			continue
		}
		for _, verb := range proceduralVerbs {
			if phrases.ContainsPhrase(always, verb) {
				return true
			}
		}
	}
	if phrases.ContainsAnyLiteral("总是", "始终") {
		return true
	}
	for _, prefer := range proceduralPreferenceTokens {
		if !phrases.Contains(prefer) {
			continue
		}
		if slices.ContainsFunc(proceduralPreferenceTargets, phrases.Contains) {
			return true
		}
	}
	if phrases.ContainsAnyLiteral("更喜欢", "优先") && slices.ContainsFunc(proceduralPreferenceTargets, phrases.ContainsLiteral) {
		return true
	}
	return false
}
