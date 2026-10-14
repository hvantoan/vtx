-- ==============================================================================
-- Database initialization for VTX Platform and Temporal
-- Logical isolation: vtx_platform vs temporal
-- ==============================================================================

-- Create temporal database if it does not exist
SELECT 'CREATE DATABASE temporal'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'temporal')\gexec

-- Grant all privileges to the default user
GRANT ALL PRIVILEGES ON DATABASE vtx_platform TO vtx_user;
GRANT ALL PRIVILEGES ON DATABASE temporal TO vtx_user;
