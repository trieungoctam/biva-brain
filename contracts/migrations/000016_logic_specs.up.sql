-- Logic spec của nhà xe (M2, S2.5.3 extract_logic_spec): "dấu vân tay" logic theo capability,
-- dựng từ tri thức đã duyệt — có trước code. Thiết kế: docs/logic-knowledge.md §4.

CREATE TABLE logic_specs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id     TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    capability      TEXT NOT NULL,
    features        JSONB NOT NULL DEFAULT '[]',   -- [{id: "fare.holiday_surcharge", params: {…}}]
    rules_text      TEXT[] NOT NULL DEFAULT '{}',  -- quy tắc bằng lời đã chuẩn hoá (so ngữ nghĩa giữa nhà xe)
    source_item_ids UUID[] NOT NULL DEFAULT '{}',  -- item tri thức đã dùng → đối chiếu stale sau này
    example_ids     UUID[] NOT NULL DEFAULT '{}',  -- logic_tests dùng làm bằng chứng hành vi
    proposed_features JSONB NOT NULL DEFAULT '[]', -- feature chưa có trong danh mục → chờ review (không tự thêm)
    implementation  JSONB,                         -- null khi chưa có code; có profile: {mode, module, hooks}
    embedding       vector,                        -- rules_text ghép (TEI); NULL → so theo từ khoá
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'stale', 'retired')),
    created_by      TEXT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (operator_id, capability)
);
CREATE INDEX logic_specs_capability ON logic_specs (capability) WHERE status = 'active';
