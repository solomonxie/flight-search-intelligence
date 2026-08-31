"""Spark batch job: clean/dedupe raw scraped flight rows before they land
in the warehouse for dbt to model.

Run: spark-submit clean_raw_flights.py --input s3://.../raw --output s3://.../clean

TODO: implement actual cleaning (dedupe by (origin, destination, airline,
depart_date, source), drop rows with a missing price, normalize currency).
"""

from pyspark.sql import SparkSession


def main() -> None:
    spark = SparkSession.builder.appName("clean_raw_flights").getOrCreate()
    # TODO: read raw scraped data, clean it, and write to the staging
    # location etl/dbt/models/staging/stg_flights.sql builds on.
    spark.stop()


if __name__ == "__main__":
    main()
