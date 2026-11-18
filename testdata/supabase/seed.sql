-- Disposable Supabase-style REST fixture. PostgreSQL's init hook inserts every
-- row here, never through PostgREST and never through the adapter. The exposed
-- roles are least-privilege:
--   anon          no table grant at all (a request without a JWT is refused);
--   authenticated SELECT only, and only rows whose owner_sub matches the JWT
--                 `sub` claim;
--   noaccess      USAGE on the schema but no table grant (a real denial).
--
-- Nothing here creates an account, a payment path, a production object or a
-- service-role/admin credential.

create schema app;

create role authenticator login password 'authenticator-pass' noinherit;
create role anon nologin;
create role authenticated nologin;
create role noaccess nologin;

grant anon, authenticated, noaccess to authenticator;

grant usage on schema app to anon, authenticated, noaccess;

-- ---------------------------------------------------------------------------
-- Owner-scoped relation: allowed rows, hidden rows and the native values.
-- ---------------------------------------------------------------------------
-- 250 rows belong to `fixture-user`. `big_number` holds 2^53 + 1 for row 1, so
-- a JSON decoder that passes through float64 is observable. `tags` is a native
-- array, `nullable` has SQL NULL and empty string, and `payload` is native
-- nested JSONB.
create table app.records (
  record_id bigint primary key,
  title text not null,
  amount numeric(12, 3) not null,
  big_number bigint not null,
  tags text[],
  nullable text,
  payload jsonb not null,
  owner_sub text not null,
  created_at timestamptz not null
);

alter table app.records enable row level security;
create policy records_owner_read
  on app.records
  for select to authenticated
  using (owner_sub = current_setting('request.jwt.claims', true)::jsonb ->> 'sub');

-- SELECT only. No INSERT, UPDATE or DELETE grant exists for any role.
grant select on app.records to authenticated;

insert into app.records
  (record_id, title, amount, big_number, tags, nullable, payload, owner_sub, created_at)
select
  g,
  'Record ' || g,
  (g::numeric) / 8,
  case when g = 1 then 9007199254740993 else g end,
  case
    when g % 4 = 0 then array['alpha', 'beta']::text[]
    when g % 4 = 1 then array['alpha']::text[]
    else null
  end,
  case
    when g % 5 = 0 then null
    when g % 7 = 0 then ''
    else 'note-' || g
  end,
  jsonb_build_object(
    'sequence', g,
    'parity', case when g % 2 = 0 then 'even' else 'odd' end,
    'nested', jsonb_build_object('type', 'value')
  ),
  'fixture-user',
  timestamptz '2024-02-01T00:00:00Z' + (g || ' minutes')::interval
from generate_series(1, 250) as g;

-- Present in the table, never visible through the fixture user's token. The
-- acceptance counts these externally and asks for one of them through Kong, so
-- an empty result is real RLS, not a short table.
insert into app.records
  (record_id, title, amount, big_number, tags, nullable, payload, owner_sub, created_at)
select
  9000 + g,
  'Hidden record ' || g,
  (g::numeric) / 8,
  g,
  array['hidden']::text[],
  'hidden',
  jsonb_build_object('sequence', g, 'hidden', true),
  'other-user',
  timestamptz '2024-03-01T00:00:00Z' + (g || ' minutes')::interval
from generate_series(1, 20) as g;

-- A second relation the authenticated role may read, so a known relation read
-- is proven independent of schema discovery. NULL and empty string stay
-- distinct.
create table app.labels (
  id bigint primary key,
  name text
);

alter table app.labels enable row level security;
create policy labels_read on app.labels for select to authenticated using (true);
grant select on app.labels to authenticated;

insert into app.labels (id, name) values
  (1, ''),
  (2, null),
  (3, 'value');

-- ---------------------------------------------------------------------------
-- Real negative relations
-- ---------------------------------------------------------------------------
-- Exists in the exposed schema with no grant and no policy: the authenticated
-- role's read is a real refusal, not an empty relation.
create table app.restricted_notes (
  id bigint primary key,
  body text not null
);

alter table app.restricted_notes enable row level security;
insert into app.restricted_notes (id, body) values (1, 'never readable');

-- The authenticated role may read this relation, but selecting a column that
-- does not exist is a native PostgREST 400 (a projection error, not a shortened
-- result).
create table app.missing_column_probe (
  record_id bigint primary key,
  title text not null
);

alter table app.missing_column_probe enable row level security;
create policy missing_column_probe_read
  on app.missing_column_probe for select to authenticated using (true);
grant select on app.missing_column_probe to authenticated;

insert into app.missing_column_probe (record_id, title) values (1, 'one'), (2, 'two');
