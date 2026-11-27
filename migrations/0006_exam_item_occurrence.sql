-- Slice 4A: answer_space_mm becomes an occurrence field. The value belongs to
-- one exam_items row (one appearance of a question in one exam), not to the
-- shared question entity, so editing it can never change another exam.
--
-- Backfill copies each referenced entity's content_json.answer_space_mm into
-- the item override. An absent or zero value stays NULL; the assembler renders
-- both as 0, so existing exams keep their exact layout.

ALTER TABLE exam_items
    ADD COLUMN answer_space_mm_override double precision
        CHECK (answer_space_mm_override IS NULL OR answer_space_mm_override >= 0),
    ADD COLUMN revision bigint NOT NULL DEFAULT 1
        CHECK (revision > 0);

UPDATE exam_items AS i
SET answer_space_mm_override = (q.content_json ->> 'answer_space_mm')::double precision
FROM questions AS q
WHERE q.question_id = i.question_id
  AND jsonb_typeof(q.content_json -> 'answer_space_mm') = 'number'
  AND (q.content_json ->> 'answer_space_mm')::double precision > 0;
