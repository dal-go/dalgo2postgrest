-- Disposable integration fixture. It seeds application tables and a read-only
-- JWT role before PostgREST starts; the adapter is never used to write.
create schema app;

create role authenticator login password 'authenticator-pass' noinherit;
create role anon nologin;
create role app_reader nologin;
create role noaccess nologin;

grant anon to authenticator;
grant app_reader to authenticator;
grant noaccess to authenticator;

grant usage on schema app to anon, app_reader, noaccess;

create table app.items (
  id bigint primary key,
  title text not null,
  qty int not null,
  big_qty bigint not null,
  price numeric(30, 2) not null,
  active boolean not null,
  created_at timestamptz not null,
  tags text[],
  doc jsonb
);

alter table app.items enable row level security;
create policy items_reader on app.items for select to app_reader using (true);

grant select on app.items to app_reader;

create table app.notes (
  id text primary key,
  body text not null
);

alter table app.notes enable row level security;
create policy notes_reader on app.notes for select to app_reader using (true);
grant select on app.notes to app_reader;

-- Three distinct storage states that must not collapse into each other:
-- a zero-length string, SQL NULL, and a non-empty value.
create table app.labels (
  id bigint primary key,
  name text
);

alter table app.labels enable row level security;
create policy labels_reader on app.labels for select to app_reader using (true);
grant select on app.labels to app_reader;

insert into app.items (id, title, qty, big_qty, price, active, created_at, tags, doc) values
  (1, 'alpha', 10, 9007199254740993, 12.50, true,  '2024-01-01T10:00:00Z', '{a,b}', '{"k":1}'),
  (2, 'beta',  20, 2,                9007199254740993.00, true,  '2024-01-02T10:00:00Z', '{b}',   '{"k":2}'),
  (3, 'gamma', 20, 3,                3.25,   false, '2024-01-03T10:00:00Z', null,    null),
  (4, 'delta', 30, 4,                1.00,   true,  '2024-01-04T10:00:00Z', '{c}',   '{"k":4}');

insert into app.notes (id, body) values ('note-1', 'first note');

insert into app.labels (id, name) values
  (1, ''),
  (2, null),
  (3, 'value'),
  (4, ''),
  (5, 'other'),
  (6, null);
