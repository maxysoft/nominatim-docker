#!/bin/bash -ex

tailpid=0
replicationpid=0
GUNICORN_PID_FILE=/tmp/gunicorn.pid
# send gunicorn logs straight to the console without buffering: https://stackoverflow.com/questions/59812009
export PYTHONUNBUFFERED=1

stopServices() {
  # Check if the replication process is active
  if [ $replicationpid -ne 0 ]; then
    echo "Shutting down replication process"
    kill $replicationpid
  fi
  if [ $tailpid -ne 0 ]; then
    kill $tailpid
  fi
  if [ -f $GUNICORN_PID_FILE ]; then
    cat $GUNICORN_PID_FILE | sudo xargs kill
  fi

  # Force exit code 0 to signal a successful shutdown to Docker
  exit 0
}
trap stopServices SIGTERM TERM INT

/app/config.sh

if id nominatim >/dev/null 2>&1; then
  echo "user nominatim already exists"
else
  useradd -m -p ${NOMINATIM_PASSWORD} nominatim
fi

IMPORT_FINISHED=${PROJECT_DIR}/import-finished

if [ ! -f ${IMPORT_FINISHED} ]; then
  /app/init.sh
  touch ${IMPORT_FINISHED}
else
  chown -R nominatim:nominatim ${PROJECT_DIR}
fi

# Wait for external PostgreSQL to be ready
echo "Waiting for PostgreSQL at ${POSTGRES_HOST}:${POSTGRES_PORT} to be ready..."
until PGPASSWORD="${NOMINATIM_PASSWORD}" psql -h "${POSTGRES_HOST}" -p "${POSTGRES_PORT}" -U "nominatim" -d "${POSTGRES_DB}" -c '\q' 2>/dev/null; do
  echo "PostgreSQL is unavailable - sleeping"
  sleep 2
done
echo "PostgreSQL is ready"

cd ${PROJECT_DIR} && sudo -E -u nominatim nominatim refresh --functions

# start continous replication process
if [ "$REPLICATION_URL" != "" ] && [ "$FREEZE" != "true" ]; then
  # run init in case replication settings changed
  sudo -E -u nominatim nominatim replication --project-dir ${PROJECT_DIR} --init
  if [ "$UPDATE_MODE" == "continuous" ]; then
    echo "starting continuous replication"
    sudo -E -u nominatim nominatim replication --project-dir ${PROJECT_DIR} &> /var/log/replication.log &
    replicationpid=${!}
  elif [ "$UPDATE_MODE" == "once" ]; then
    echo "starting replication once"
    sudo -E -u nominatim nominatim replication --project-dir ${PROJECT_DIR} --once &> /var/log/replication.log &
    replicationpid=${!}
  elif [ "$UPDATE_MODE" == "catch-up" ]; then
    echo "starting replication once in catch-up mode"
    sudo -E -u nominatim nominatim replication --project-dir ${PROJECT_DIR} --catch-up &> /var/log/replication.log &
    replicationpid=${!}
  else
    echo "skipping replication"
  fi
fi

# No local PostgreSQL logs to tail since we're using external DB
tailpid=0

if [ "$WARMUP_ON_STARTUP" = "true" ]; then
  export NOMINATIM_QUERY_TIMEOUT=600
  export NOMINATIM_REQUEST_TIMEOUT=3600
  if [ "$REVERSE_ONLY" = "true" ]; then
    echo "Warm database caches for reverse queries"
    sudo -H -E -u nominatim nominatim admin --warm --reverse > /dev/null
  else
    echo "Warm database caches for search and reverse queries"
    sudo -H -E -u nominatim nominatim admin --warm > /dev/null
  fi
  export NOMINATIM_QUERY_TIMEOUT=10
  export NOMINATIM_REQUEST_TIMEOUT=60
  echo "Warming finished"
else
  echo "Skipping cache warmup"
fi

# Set default number of workers if not specified
if [ -z "$GUNICORN_WORKERS" ]; then
  GUNICORN_WORKERS=$(nproc)
fi

echo "Starting Gunicorn with $GUNICORN_WORKERS workers"

echo "--> Nominatim is ready to accept requests"

cd "$PROJECT_DIR"
sudo -u nominatim gunicorn \
  --bind :8080 \
  --pid $GUNICORN_PID_FILE \
  --workers $GUNICORN_WORKERS \
  --daemon \
  --enable-stdio-inheritance \
  --worker-class uvicorn.workers.UvicornWorker \
  --factory \
  --access-logfile - \
  nominatim_api.server.falcon.server:run_wsgi

# Wait for the PID file to be created
while [ ! -f $GUNICORN_PID_FILE ]; do
  sleep 1
done

# Get the PID and wait for the Gunicorn process to exit
GUNICORN_PID=$(cat $GUNICORN_PID_FILE)

# Wait for the Gunicorn process to exit
while kill -0 $GUNICORN_PID 2>/dev/null; do
  sleep 5
done