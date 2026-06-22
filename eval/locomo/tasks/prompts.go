package tasks

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func qaAnswerMessages(item dataset.QAItem, pack *memory.ContextPack) []llm.Message {
	return []llm.Message{
		model.NewTextMessage(model.RoleSystem, qaAnswerSystemPrompt()),
		model.NewTextMessage(model.RoleUser, qaAnswerUserPrompt(item, pack)),
	}
}

func qaAnswerSystemPrompt() string {
	return `You answer questions using only the provided source-message context.

Context format:
- The user message contains a Source-message context block.
- ` + "`Retrieved source messages`" + ` are the primary candidate support for the answer.
- ` + "`Recent source messages`" + ` are supplemental recent context and may be unrelated.
- Each source message has a metadata line followed by ` + "`Text:`" + `. Use the text as the message content.
- Content-bearing metadata can also be evidence when it describes the source message content, including ` + "`image_caption`" + `, ` + "`caption`" + `, ` + "`blip_caption`" + `, and ` + "`query`" + `.
- ` + "`session_date_time`" + ` / ` + "`session_datetime`" + ` anchor relative time expressions.
- ` + "`speaker`" + ` is the real speaker.
- ` + "`dia_id`" + ` and ` + "`seq`" + ` only distinguish messages; never include them in the answer.

Use a single policy for all questions:
- Answer only from information supported by the source-message context.
- First identify every constraint in the question: subject/person, relationship, event/object, time, and requested answer type.
- Treat support as sufficient only when the same source message or a compatible set of source messages satisfies those constraints. Related keywords alone are not enough.
- Use only source messages about the same subject, relationship, or event; do not transfer similar facts from another person, relationship, event, or object.
- Do not require exact phrase overlap. If the same subject and constraints are supported by message text or content-bearing metadata, answer from that support.
- Use this answerability order: direct support, compatible multi-message support, reasonable same-subject inference, supported partial answer, then refusal.
- If the context directly supports an answer, give the shortest complete answer.
- If the context supports a reasonable inference for the same subject and constraints, answer with that inference. For likely/would/probably/preference questions, answer the supported tendency instead of refusing merely because it is inferential.
- If the question asks for several facts, a list, or a count, scan all source messages about the same subject/event before answering. Collect every supported required part, deduplicate equivalent parts, and do not stop after the first matching message. Give a count only when the supported items can be enumerated.
- If the context supports only part of a multi-part answer, provide the supported partial answer instead of refusing. Do not refuse just because one requested part is missing.
- For yes/no questions, answer "No" only when the context affirmatively supports the negation, such as showing that a different subject owns the item or performed the action. Do not answer "No" merely because the context lacks support for "Yes"; use "No information available." when the context is silent.
- For time questions, bind relative references to the source message's session date/time. Convert to an absolute date, month, or year when the conversion is clear. Preserve the source anchored expression when exact calendar conversion is uncertain, such as "the Friday before 15 July 2023". Do not refuse because the support gives only a year or month when that granularity answers the question.
- Say exactly "No information available." only as the final fallback: when there is no same-subject/same-event support for the question, or when all related information fails the question's subject, relationship, event, and time constraints.
- Return only answer text; do not include reasoning, message IDs, context, source labels, metadata fields, or process notes.

Fictional mini-examples:

Context:
Retrieved source messages:
[A] session_date_time=15 July 2023 09:00 | speaker=Mira
Text: Mira said she bought the hiking boots last Friday.
Question: When did Mira buy the hiking boots?
Answer: the Friday before 15 July 2023

Context:
Retrieved source messages:
[A] session_date_time=8 May 2023 20:00 | speaker=Ivo
Text: Ivo said the orchestra audition was yesterday.
Question: When was Ivo's orchestra audition?
Answer: 7 May 2023

Context:
Retrieved source messages:
[A] session_date_time=25 May 2023 12:00 | speaker=Mel
Text: Mel said she ran the charity race on the Sunday before this chat.
Question: When did Mel run the charity race?
Answer: the Sunday before 25 May 2023

Context:
Retrieved source messages:
[A] speaker=Noor
Text: Noor said the trip needs sunscreen.
[B] speaker=Eli
Text: Eli said the trip also needs bus tickets.
Question: What items are needed for the trip?
Answer: sunscreen and bus tickets

Context:
Retrieved source messages:
[A] speaker=Owen
Text: Owen camped at the beach during spring break.
[B] speaker=Owen
Text: Owen later camped in the mountains with his cousins.
[C] speaker=Owen
Text: Owen also mentioned a forest campsite near the lake.
Question: Where has Owen camped?
Answer: beach, mountains, and forest

Context:
Retrieved source messages:
[A] speaker=Jae
Text: Jae chose the blue notebook for the workshop.
Question: What notebook and marker color did Jae choose?
Answer: the blue notebook

Context:
Retrieved source messages:
[A] speaker=Sara
Text: Sara collects classic children's books and said Dr. Seuss was one of her childhood favorites.
Question: Would Sara likely have Dr. Seuss books on her bookshelf?
Answer: likely yes

Context:
Retrieved source messages:
[A] speaker=Tala | image_caption=a poster that says "Community Garden Day" | query=garden event poster
Text: Tala shared a photo from the neighborhood bulletin board.
Question: What did the poster say?
Answer: Community Garden Day

Context:
Retrieved source messages:
[A] speaker=Lena
Text: Lena said her brother Kai moved to Boston.
[B] speaker=Lena
Text: Lena said her sister Nia moved to Portland.
Question: Where did Lena's sister move?
Answer: Portland

Context:
Retrieved source messages:
[A] speaker=Ravi
Text: Ravi talked about planting basil.
Question: What concert did Ravi attend?
Answer: No information available.

Context:
Retrieved source messages:
[A] speaker=Iris
Text: Iris said her grandmother gave her a silver locket.
[B] speaker=Omar
Text: Omar asked about family keepsakes.
Question: What did Omar's grandmother give him?
Answer: No information available.

Context:
Retrieved source messages:
[A] speaker=Rina
Text: Rina said the clay vase was made by Mateo, not by Rina.
Question: Did Rina make the clay vase?
Answer: No

Return only the answer text.`
}

func qaAnswerUserPrompt(item dataset.QAItem, pack *memory.ContextPack) string {
	return `Source-message context:
` + renderContext(pack) + `

Question:
` + item.Question + `

Answer:`
}

func eventSummaryMessages(pack *memory.ContextPack, query string) []llm.Message {
	return []llm.Message{
		model.NewTextMessage(model.RoleSystem, `Write concise event summaries grounded only in the provided source-message context.

Use only supported facts. Preserve the requested session and speaker when provided. Do not mention retrieval, context, evidence IDs, the judge, or scoring.

Return only the event summary text.`),
		model.NewTextMessage(model.RoleUser, "Source-message context:\n"+renderContext(pack)+"\n\nRequest:\n"+query),
	}
}

func dialogAnswerMessages(pack *memory.ContextPack, c dataset.DialogCase) []llm.Message {
	query := strings.TrimSpace(c.Query)
	if query == "" {
		query = "Generate the next dialog response for this image caption."
	}
	return []llm.Message{
		model.NewTextMessage(model.RoleSystem, `This is a caption-proxy multimodal dialog task.

Use only the provided source-message context and image metadata. Generate the next response without claiming to inspect the original image directly. Do not mention retrieval, context, evidence IDs, the judge, or scoring.

Return only the dialog response.`),
		model.NewTextMessage(model.RoleUser, fmt.Sprintf("Source-message context:\n%s\n\nCaption:\n%s\n\nQuery:\n%s", renderContext(pack), c.Caption, query)),
	}
}
