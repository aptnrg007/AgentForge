-- Seed data for examples/sql-analyst.yaml.
--   sqlite3 /tmp/shop.db < examples/data/shop.sql
--   export AGENTFORGE_SQLITE_DB=/tmp/shop.db

CREATE TABLE customers (
  id    INTEGER PRIMARY KEY,
  name  TEXT NOT NULL,
  email TEXT NOT NULL
);

CREATE TABLE orders (
  id          INTEGER PRIMARY KEY,
  customer_id INTEGER NOT NULL REFERENCES customers(id),
  total_cents INTEGER NOT NULL,
  placed_at   TEXT NOT NULL
);

INSERT INTO customers (id, name, email) VALUES
  (1, 'Asha Rao',     'asha@example.com'),
  (2, 'Wei Chen',     'wei@example.com'),
  (3, 'Maria Santos', 'maria@example.com');

INSERT INTO orders (id, customer_id, total_cents, placed_at) VALUES
  (1, 1, 4200,  '2026-06-01'),
  (2, 1, 1899,  '2026-06-15'),
  (3, 2, 9999,  '2026-07-02'),
  (4, 3, 500,   '2026-07-20'),
  (5, 1, 12000, '2026-08-05');
