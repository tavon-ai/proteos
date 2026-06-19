-- Machine templates: record which catalog template a machine was created from.
-- Descriptive/audit only — the load-bearing fields stay kernel_ref / rootfs_ref /
-- resource_spec, already on the row. Nullable so existing machines (created before
-- templates) keep template_id = NULL and render as a legacy machine; no backfill.
ALTER TABLE machines ADD COLUMN template_id text;
