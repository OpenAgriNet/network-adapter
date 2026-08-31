API_KEY=XXXXXX
Pagination & response limits

Some One Call API 4.0 endpoints may return large datasets, especially when requesting forecast or historical weather data. To improve API performance and ensure efficient data delivery, responses can be split into multiple pages.

When pagination is applied, the API response includes a fully prepared URL for retrieving the next page of data.

How pagination works
Send a request to the API endpoint.
Receive a response containing weather data and, if additional data is available, next or prev fields.
Use the URL provided in the next or prev field to request the next or previous page of results.
Example of API response
{
  "lat": 51.5,
  "lon": -0.1,
  "timezone": "Europe/London",
  "timezone_offset": 3600,
  "data": [
    {
      "dt": 1777460400,
      "sunrise": 1777437375,
      "sunset": 1777490344,
      "moonrise": 1777482960,
      "moonset": 1777433400,
      "moon_phase": 0.43,
      ...
	  }
    ...
  ],
  "prev": "https://api.openweathermap.org/data/4.0/onecall/timeline/1day?cnt=10&lat=51.5000&lon=-0.1000&start=1776596400&appid={API key}",
  "next": "https://api.openweathermap.org/data/4.0/onecall/timeline/1day?cnt=10&lat=51.5000&lon=-0.1000&start=1778324400&appid={API key}"
}
Toggle
Copy Icon
Pagination parameters
One Call API 4.0 uses timeline-based pagination to navigate through weather data forward and backward in time.

Parameters

start

UTC date and time used as the starting point of the timeline. Records before and after this timestamp can be accessed using pagination links. If start is not specified, the current UTC time is used by default.

next

URL for retrieving the next portion of records forward in the timeline.

prev

URL for retrieving the previous portion of records backward in the timeline.

Each endpoint returns a fixed maximum number of records per response. Please refer to the corresponding endpoint documentation for response record limits.

Response record limits
Each One Call API 4.0 endpoint has a maximum number of records that can be returned in a single response. These limits are described in the corresponding endpoint sections of the documentation.

If the available dataset exceeds the response limit, the API response includes next and/or prev URLs that can be used to continue retrieving data across the timeline.

Please note that each paginated request made using the next or prev URLs is counted as a separate API call according to your subscription plan.

Current weather data

The API endpoint returns current weather conditions for a specific location with core meteorological parameters such as temperature, feels-like temperature, pressure, humidity, dew point, UV index, cloud cover, visibility, wind speed, wind direction, sunrise and sunset times, and weather condition descriptors with icons. This endpoint is useful for apps and services that need an instant snapshot of weather at a location.

If you are interested in other functionality on One Call API 4.0, please check Product concept to follow the right section.

How to make an API call
API call
https://api.openweathermap.org/data/4.0/onecall/current?lat={lat}&lon={lon}&appid={API key}
Copy Icon
Parameters

lat

required

Latitude, decimal (-90; 90). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

lon

required

Longitude, decimal (-180; 180). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

appid

required

Your unique API key (you can always find it on your account page under the "API key" tab)

units

optional

Units of measurement. standard, metric and imperial units are available. If you do not use the units parameter, standard units will be applied by default. Learn more

lang

optional

You can use the lang parameter to get the output in your language. Learn more

Example of API call
Before making an API call, please note that One Call 4.0 is included in the "One Call by Call" subscription only. Learn more

Example of API call
https://api.openweathermap.org/data/4.0/onecall/current?lat=52.2297&lon=21.0122&units=metric&lang=en&appid={API key} 
Copy Icon
Example of API response
{
  "lat": 51.5,
  "lon": -0.1,
  "timezone": "Europe/London",
  "timezone_offset": 3600,
  "data": [
    {
      "dt": 1777449371,
      "sunrise": 1777437375,
      "sunset": 1777490344,
      "temp": 286.42,
      "feels_like": 285.32,
      "pressure": 1024,
      "humidity": 58,
      "dew_point": 278.34,
      "uvi": 1.55,
      "clouds": 0,
      "visibility": 10000,
      "wind_speed": 8.23,
      "wind_deg": 70,
      "weather": [
        {
          "id": 800,
          "main": "Clear",
          "description": "sky is clear",
          "icon": "01d"
        }
      ]
	  "alerts": [
		"8B46C632-DCA7-44D7-8BDF-02445621BAFF",
		"29F58A35-BB91-4A73-9F46-9FC64BDF604F",
		...
	]
    }
  ]
}
Toggle
Copy Icon
Fields in API response
If you do not see some of the parameters in your API response, it means these weather phenomena did not occur at the time of measurement for the selected city or location. Only measured or calculated data is displayed in the API response.

Current weather endpoint returns 1 record in the API response.

lat Latitude of the location, decimal (−90; 90)
lon Longitude of the location, decimal (-180; 180)
timezone Timezone name for the requested location
timezone_offset Shift in seconds from UTC
data.dt Current time, Unix, UTC
data.sunrise Sunrise time, Unix, UTC. For polar areas in midnight sun and polar night periods this parameter is not returned in the response
data.sunset Sunset time, Unix, UTC. For polar areas in midnight sun and polar night periods this parameter is not returned in the response
data.temp Temperature. Units - default: kelvin, metric: Celsius, imperial: Fahrenheit. How to change units used
data.feels_like Temperature. This temperature parameter accounts for the human perception of weather. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit.
data.pressure Atmospheric pressure at sea level, hPa
data.humidity Humidity, %
data.dew_point Atmospheric temperature (varying according to pressure and humidity) below which water droplets begin to condense and dew can form. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit
data.clouds Cloudiness, %
data.uvi Current UV index.
data.visibility Average visibility, metres. The maximum value of the visibility is 10 km
data.wind_speed Wind speed. Units – default: metre/sec, metric: metre/sec, imperial: miles/hour. How to change units used
data.wind_gust (where available) Wind gust. Units – default: metre/sec, metric: metre/sec, imperial: miles/hour. How to change units used
data.wind_deg Wind direction, degrees (meteorological)
data.rain
data.rain.1h (where available) Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.snow
data.snow.1h (where available) Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.weather
data.weather.id Weather condition id
data.weather.main Group of weather parameters (Rain, Snow etc.)
data.weather.description Weather condition within the group (full list of weather conditions). Get the output in your language
data.weather.icon Weather icon id. How to get icons
data.alerts Array of weather alert IDs associated with the requested location and time. Each ID can be used to retrieve detailed information about the corresponding alert via the Weather Alert detailed information endpoint. National weather alerts are provided in English by default. Please note that some agencies provide the alert’s description only in a local language.
1 minute step timeline

How to make an API call
API call
https://api.openweathermap.org/data/4.0/onecall/timeline/1min?lat={lat}&lon={lon}&appid={API key}
Copy Icon
Parameters

lat

required

Latitude, decimal (-90; 90). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

lon

required

Longitude, decimal (-180; 180). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

appid

required

Your unique API key (you can always find it on your account page under the "API key" tab)

units

optional

Units of measurement. standard, metric and imperial units are available. If you do not use the units parameter, standard units will be applied by default. Learn more

lang

optional

You can use the lang parameter to get the output in your language. Learn more

Example of API call
Example of API call
https://api.openweathermap.org/data/4.0/onecall/timeline/1min?lat=51.5&lon=-0.1&appid={API key}
Copy Icon
Example of API response
{
  "lat": 51.5,
  "lon": -0.1,
  "timezone": "Europe/London",
  "timezone_offset": 3600,
  "data": [
    {
      "dt": 1777451940,
      "precipitation": 0,
	  "alerts": [
		"8B46C632-DCA7-44D7-8BDF-02445621BAFF",
		"29F58A35-BB91-4A73-9F46-9FC64BDF604F",
		...
	],
...
  ]
  
}
Toggle
Copy Icon
The 1-minute timeline returns up to 60 records in the API response.

Fields in API response
lat Latitude of the location, decimal (−90; 90)
lon Longitude of the location, decimal (-180; 180)
timezone Timezone name for the requested location
timezone_offset Shift in seconds from UTC
data 
data.dt Time of the forecasted data, unix, UTC
data.precipitation Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.alerts Array of weather alert IDs associated with the requested location and time. Each ID can be used to retrieve detailed information about the corresponding alert via the Weather Alert detailed information endpoint. National weather alerts are provided in English by default. Please note that some agencies provide the alert’s description only in a local language.
15 minutes step timeline

How to make an API call
API call
https://api.openweathermap.org/data/4.0/onecall/timeline/15min?lat={lat}&lon={lon}&appid={API key}
Copy Icon
Parameters

lat

required

Latitude, decimal (-90; 90). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

lon

required

Longitude, decimal (-180; 180). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

appid

required

Your unique API key (you can always find it on your account page under the "API key" tab)

units

optional

Units of measurement. standard, metric and imperial units are available. If you do not use the units parameter, standard units will be applied by default. Learn more

lang

optional

You can use the lang parameter to get the output in your language. Learn more

Example of API call
Example of API call
https://api.openweathermap.org/data/4.0/onecall/timeline/15min?lat=51.5&lon=-0.1&appid={API key}
Copy Icon
Example of API response
{
  "lat": 51.5,
  "lon": -0.1,
  "timezone": "Europe/London",
  "timezone_offset": 3600,
  "data": [
    {
      "dt": 1777452300,
      "temp": 287.95,
      "feels_like": 286.75,
      "pressure": 1024,
      "humidity": 48,
      "dew_point": 277.2,
      "uvi": 2.36,
      "clouds": 0,
      "visibility": 10000,
      "wind_speed": 7.41,
      "wind_deg": 70,
      "pop": 0,
      "weather": [
        {
          "id": 800,
          "main": "Clear",
          "description": "sky is clear",
          "icon": "01d"
        }
      ],
	  "alerts": [
		"8B46C632-DCA7-44D7-8BDF-02445621BAFF",
		"29F58A35-BB91-4A73-9F46-9FC64BDF604F",
		...
	]
    },
	...
  ],
"next": "https://api.openweathermap.org/data/4.0/onecall/timeline/15min?lat=51.5000&lon=-0.1000&start=1777497300&appid={API key}"
}
Toggle
Copy Icon
The 15-minute timeline returns up to 50 records in a single API response. To retrieve the full dataset, please check the next parameter in the API response. If present, it contains a fully prepared URL for requesting the next portion of records. If the next parameter is not returned, it means the full dataset has already been retrieved. For more details, see the Pagination & response limits section.

Fields in API response
lat Latitude of the location, decimal (−90; 90)
lon Longitude of the location, decimal (-180; 180)
timezone Timezone name for the requested location
timezone_offset Shift in seconds from UTC
data
data.dt Time of the forecasted data, Unix, UTC
data.temp Temperature. Units - default: kelvin, metric: Celsius, imperial: Fahrenheit. How to change units used
data.feels_like Temperature. This temperature parameter accounts for the human perception of weather. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit.
data.pressure Atmospheric pressure on the sea level, hPa
data.humidity Humidity, %
data.dew_point Atmospheric temperature (varying according to pressure and humidity) below which water droplets begin to condense and dew can form. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit
data.clouds Cloudiness, %
data.uvi UV index.
data.visibility Average visibility, metres. The maximum value of the visibility is 10 km
data.wind_speed Wind speed. Wind speed. Units – default: metre/sec, metric: metre/sec, imperial: miles/hour. How to change units used
data.wind_gust (where available) Wind gust. Units – default: metre/sec, metric: metre/sec, imperial: miles/hour. How to change units used
data.wind_deg Wind direction, degrees (meteorological)
data.rain
data.rain.1h (where available) Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.snow
data.snow.1h (where available) Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.weather
data.weather.id Weather condition id
data.weather.main Group of weather parameters (Rain, Snow etc.)
data.weather.description Weather condition within the group (full list of weather conditions). Get the output in your language
data.weather.icon Weather icon id. How to get icons
data.alerts Array of weather alert IDs associated with the requested location and time. Each ID can be used to retrieve detailed information about the corresponding alert via the Weather Alert detailed information endpoint. National weather alerts are provided in English by default. Please note that some agencies provide the alert’s description only in a local language.
prev API-generated request URL that can be used to retrieve the previous portion of data relative to the current time range. This link allows navigation to earlier records using the same query parameters.
next API-generated request URL that can be used to retrieve the next portion of data relative to the current time range. This link allows navigation to later records using the same query parameters.
1 hour step timeline

How to make an API call
API call
https://api.openweathermap.org/data/4.0/onecall/timeline/1h?lat={lat}&lon={lon}&appid={API key}
Copy Icon
Parameters

lat

required

Latitude, decimal (-90; 90). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

lon

required

Longitude, decimal (-180; 180). If you need to automatically convert city names and ZIP codes into geographic coordinates, or vice versa, please use our Geocoding API

appid

required

Your unique API key (you can always find it on your account page under the "API key" tab)

units

optional

Units of measurement. standard, metric and imperial units are available. If you do not use the units parameter, standard units will be applied by default. Learn more

lang

optional

You can use the lang parameter to get the output in your language. Learn more

Example of API call
Example of API call
https://api.openweathermap.org/data/4.0/onecall/timeline/1h?lat=51.5&lon=-0.1&appid={API key}
Copy Icon
Example of API response
To view the API response, expand the example by clicking the triangle.
Toggle
Copy Icon
The 1-hour timeline returns up to 20 records in a single API response. To retrieve the full dataset, please use the next and prev parameters returned in the API response. These parameters contain fully prepared URLs for requesting the following or previous portions of records within the timeline. If the next or prev parameter is not returned, it means there are no additional records available in that direction. For more details, see the Pagination & response limits section.

Fields in API response
lat Latitude of the location, decimal (−90; 90)
lon Longitude of the location, decimal (-180; 180)
timezone Timezone name for the requested location
timezone_offset Shift in seconds from UTC
data 
data.dt Time of the forecasted data, Unix, UTC
data.temp Temperature. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit. How to change units used
data.feels_like Temperature. This accounts for the human perception of weather. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit.
data.pressure Atmospheric pressure on the sea level, hPa
data.humidity Humidity, %
data.dew_point Atmospheric temperature (varying according to pressure and humidity) below which water droplets begin to condense and dew can form. Units – default: kelvin, metric: Celsius, imperial: Fahrenheit.
data.uvi UV index
data.clouds Cloudiness, %
data.visibility Average visibility, metres. The maximum value of the visibility is 10 km
data.wind_speed Wind speed. Units – default: metre/sec, metric: metre/sec, imperial: miles/hour.How to change units used
data.wind_gust (where available) Wind gust. Units – default: metre/sec, metric: metre/sec, imperial: miles/hour. How to change units used
data.wind_deg Wind direction, degrees (meteorological)
data.pop Probability of precipitation. The values of the parameter vary between 0 and 1, where 0 is equal to 0%, 1 is equal to 100%
data.rain
data.rain.1h (where available) Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.snow
data.snow.1h (where available) Precipitation, mm/h. Please note that only mm/h as units of measurement are available for this parameter
data.weather
hourly.weather.id Weather condition id
hourly.weather.main Group of weather parameters (Rain, Snow etc.)
hourly.weather.description Weather condition within the group (full list of weather conditions). Get the output in your language
hourly.weather.icon Weather icon id. How to get icons
data.alerts Array of weather alert IDs associated with the requested location and time. Each ID can be used to retrieve detailed information about the corresponding alert via the Weather Alert detailed information endpoint. National weather alerts are provided in English by default. Please note that some agencies provide the alert’s description only in a local language.
prev API-generated request URL that can be used to retrieve the previous portion of data relative to the current time range. This link allows navigation to earlier records using the same query parameters.
next API-generated request URL that can be used to retrieve the next portion of data relative to the current time range. This link allows navigation to later records using the same query parameters.