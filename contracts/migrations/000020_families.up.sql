-- Họ logic (M3, S3.4.3) + cache tương đồng (S3.2.3 promote logic): gom cụm logic spec của các nhà xe
-- theo capability; họ có >= 3 nhà xe cùng hook/custom là ứng viên promote lên tham số/hook chuẩn.

CREATE TABLE logic_families (
    id                        TEXT PRIMARY KEY,            -- <capability>.<slug>
    capability                TEXT NOT NULL,
    name                      TEXT NOT NULL,
    centroid_features         TEXT[] NOT NULL DEFAULT '{}', -- feature chung của các thành viên
    members                   TEXT[] NOT NULL DEFAULT '{}', -- operator_id
    recommended_implementation JSONB,                       -- mode/module phổ biến nhất trong họ
    promote_candidate         BOOLEAN NOT NULL DEFAULT false, -- >= 3 thành viên cùng hook/custom
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE logic_similarity (
    capability       TEXT NOT NULL,
    operator_a       TEXT NOT NULL,
    operator_b       TEXT NOT NULL,
    spec_score       DOUBLE PRECISION NOT NULL DEFAULT 0,
    code_score       DOUBLE PRECISION NOT NULL DEFAULT 0,  -- S3.4.3 phần code: embedding chunks (sau)
    behavior_pass_rate DOUBLE PRECISION,                   -- run_examples_against (M3, sau)
    computed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (capability, operator_a, operator_b),
    CHECK (operator_a < operator_b)
);
