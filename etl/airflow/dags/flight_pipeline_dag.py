"""Orchestrates the nightly flight-data pipeline:
collector scrape -> Spark clean -> dbt build.

TODO: replace the BashOperator stubs below with real tasks once each
stage is implemented.
"""

from datetime import datetime

from airflow import DAG
from airflow.operators.bash import BashOperator

with DAG(
    dag_id="flight_pipeline",
    start_date=datetime(2026, 1, 1),
    schedule="@daily",
    catchup=False,
) as dag:
    scrape = BashOperator(task_id="run_collector", bash_command="echo TODO: trigger cmd/collector")
    clean = BashOperator(task_id="spark_clean", bash_command="echo TODO: spark-submit etl/spark/clean_raw_flights.py")
    transform = BashOperator(task_id="dbt_build", bash_command="echo TODO: dbt build --project-dir etl/dbt")

    scrape >> clean >> transform
